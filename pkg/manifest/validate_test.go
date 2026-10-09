package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name string, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSchemasCompile(t *testing.T) {
	for _, kind := range []FileKind{KindTable, KindView, KindChangeset} {
		if _, err := schemaFor(kind); err != nil {
			t.Errorf("%s schema: %s", kind, err)
		}
	}
}

// the history tables are created from embedded table definitions, so they must be valid too
func TestEmbeddedHistoryTablesValid(t *testing.T) {
	reader = osReadWrapper{}
	for name, raw := range map[string][]byte{"history": SkimaSchemaHistoryTable, "statements": SkimaSchemaStatementsTable} {
		result := ValidateFile(writeTemp(t, name+"-table.json", string(raw)), KindTable)
		if !result.Valid {
			t.Errorf("embedded %s table is invalid: %+v", name, result.Issues)
		}
	}
}

func TestValidateFile(t *testing.T) {
	reader = osReadWrapper{}
	cases := []struct {
		name     string
		kind     FileKind
		doc      string
		location string // expected issue location - empty when the document is valid
		message  string // substring of the expected issue message
	}{
		{"valid table", KindTable, `{"$schema":"x","name":"t","columns":[{"name":"id","datatype":"integer","nullable":false,"primarykey":true,"generate":"always"}],
			"foreign-keys":[{"name":"f","columns":["id"],"reference":{"table":"p","columns":["id"]},"deleterule":"cascade"}],
			"indexes":[{"name":"i","columns":["id"],"unique":true},{"name":"j","ddl":"create index j on t (id)"}],
			"triggers":[{"name":"tr","ddl":"create trigger ..."}],
			"constraints":[{"name":"u","type":"unique","columns":["id"]},{"name":"c","type":"check","condition":"id > 0"}]}`, "", ""},
		{"misspelled property", KindTable, `{"name":"t","columns":[{"name":"id","datatype":"integer","nullabel":false}]}`, "/columns/0", "'nullabel' not allowed"},
		{"wrong casing", KindTable, `{"name":"t","columns":[{"name":"id","datatype":"integer","Nullable":false}]}`, "/columns/0", "'Nullable' not allowed"},
		{"no columns", KindTable, `{"name":"t","columns":[]}`, "/columns", "minItems"},
		{"generate without primarykey", KindTable, `{"name":"t","columns":[{"name":"id","datatype":"integer","generate":"always"}]}`, "/columns/0", "generate requires primarykey"},
		{"check without condition", KindTable, `{"name":"t","columns":[{"name":"id","datatype":"int"}],"constraints":[{"name":"c","type":"check"}]}`, "/constraints/0", "missing property 'condition'"},
		{"index without columns or ddl", KindTable, `{"name":"t","columns":[{"name":"id","datatype":"int"}],"indexes":[{"name":"i"}]}`, "/indexes/0", "set columns, or the full create index"},
		{"bad delete rule", KindTable, `{"name":"t","columns":[{"name":"id","datatype":"int"}],"foreign-keys":[{"name":"f","columns":["id"],"reference":{"table":"p","columns":["id"]},"deleterule":"nuke"}]}`, "/foreign-keys/0/deleterule", "value must be one of"},
		{"valid view", KindView, `{"name":"v","columns":["a"],"sql":"select a from t"}`, "", ""},
		{"view without sql", KindView, `{"name":"v","columns":["a"]}`, "", "missing property 'sql'"},
		{"invalid JSON", KindView, "{\n  \"name\": \"v\",\n}", "", "line 3, column 1"},
		{"valid changeset", KindChangeset, `{"version":"1.0.1","description":"d","transaction":false,"deltas":[
			{"object":"table","action":"add","name":"t","pre":{"sql":["select 1"]},"post":{"script":"p.sql"}},
			{"object":"index","action":"drop","name":"i"},
			{"object":"trigger","action":"add","name":"tr","reference":"t"},
			{"object":"fk","action":"add","name":"f","reference":"t"},
			{"object":"fk","action":"add","tablename":"t","def":{"name":"f","columns":["a"],"reference":{"table":"p","columns":["id"]}}},
			{"object":"column","action":"add","name":"c","reference":"t","def":{"name":"c","datatype":"text","nullable":true}},
			{"object":"column","action":"rename","name":"c","reference":"t","newname":"d"},
			{"object":"column","action":"alter","name":"c","reference":"t","mods":{"nullable":true,"datatype":false}},
			{"object":"script","description":"s","sql":["insert into t values (1)"],"bypass":["23505"]},
			{"object":"script","description":"s","script":"backfill.sql"}]}`, "", ""},
		{"unknown object", KindChangeset, `{"deltas":[{"object":"colum","action":"add","name":"c"}]}`, "/deltas/0/object", "value must be one of"},
		{"missing object", KindChangeset, `{"deltas":[{"action":"add","name":"c"}]}`, "/deltas/0", "missing property 'object'"},
		{"bad action", KindChangeset, `{"deltas":[{"object":"table","action":"rename","name":"t"}]}`, "/deltas/0/action", "value must be one of"},
		{"index add without reference", KindChangeset, `{"deltas":[{"object":"index","action":"add","name":"i"}]}`, "/deltas/0", "missing property 'reference'"},
		{"rename without newname", KindChangeset, `{"deltas":[{"object":"column","action":"rename","name":"c","reference":"t"}]}`, "/deltas/0", "missing property 'newname'"},
		{"two mods", KindChangeset, `{"deltas":[{"object":"column","action":"alter","name":"c","reference":"t","mods":{"nullable":true,"datatype":true}}]}`, "/deltas/0/mods", "exactly one of datatype, nullable or default"},
		{"mods without alter", KindChangeset, `{"deltas":[{"object":"column","action":"drop","name":"c","reference":"t","mods":{"nullable":true}}]}`, "/deltas/0", "mods is only used when the action is alter"},
		{"sql and script", KindChangeset, `{"deltas":[{"object":"script","sql":["select 1"],"script":"a.sql"}]}`, "/deltas/0", "exactly one of sql or script"},
		{"hook with sql and script", KindChangeset, `{"deltas":[{"object":"table","action":"add","name":"t","pre":{"sql":["x"],"script":"y.sql"}}]}`, "/deltas/0/pre", "exactly one of sql or script"},
		{"fk def with reference", KindChangeset, `{"deltas":[{"object":"fk","action":"add","tablename":"t","reference":"t","def":{"name":"f","columns":["a"],"reference":{"table":"p","columns":["id"]}}}]}`, "/deltas/0", "remove name and reference"},
		{"bad bypass code", KindChangeset, `{"deltas":[{"object":"script","sql":["select 1"],"bypass":["dup"]}]}`, "/deltas/0/bypass/0", "does not match pattern"},
		{"unknown changeset property", KindChangeset, `{"deltas":[],"transactional":false}`, "", "'transactional' not allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := ValidateFile(writeTemp(t, "file.json", c.doc), c.kind)
			if c.message == "" {
				if !result.Valid {
					t.Fatalf("EXPECTED valid  ACTUAL issues: %+v", result.Issues)
				}
				return
			}
			if result.Valid {
				t.Fatalf("EXPECTED an issue containing %q  ACTUAL: valid", c.message)
			}
			for _, issue := range result.Issues {
				if issue.Location == c.location && strings.Contains(issue.Message, c.message) {
					return
				}
			}
			t.Errorf("EXPECTED issue at %q containing %q  ACTUAL: %+v", c.location, c.message, result.Issues)
		})
	}
}

func TestKindOf(t *testing.T) {
	cases := map[string]FileKind{
		"state/users-table.json":  KindTable,
		"state/active-view.json":  KindView,
		"deltas/v1_0_2.json":      KindChangeset,
		"/abs/deltas/v2_0_0.json": KindChangeset,
		"state/notes.json":        "",
		"deltas/scripts/x.sql":    "",
	}
	for path, expected := range cases {
		kind, ok := KindOf(path)
		if kind != expected || ok != (expected != "") {
			t.Errorf("KindOf(%s) - EXPECTED: %q  ACTUAL: %q (%t)", path, expected, kind, ok)
		}
	}
}

func TestValidateManifest(t *testing.T) {
	reader = osReadWrapper{}
	workdir := t.TempDir()
	files := map[string]string{
		"state/a-table.json":  `{"name":"a","columns":[{"name":"id","datatype":"integer"}]}`,
		"state/v-view.json":   `{"name":"v","columns":["id"],"sql":"select id from a"}`,
		"state/b-tables.json": `{"name":"b","columns":[{"name":"id","datatype":"integer"}]}`,
		"deltas/v1_0_10.json": `{"deltas":[{"object":"table","action":"drop","name":"a"}]}`,
		"deltas/v1_0_9.json":  `{"deltas":[{"object":"table","action":"add"}]}`,
	}
	for name, content := range files {
		path := filepath.Join(workdir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	results, err := ValidateManifest(workdir)
	if err != nil {
		t.Fatal(err)
	}
	var summary []string
	for _, r := range results {
		summary = append(summary, fmt.Sprintf("%s=%t", filepath.Base(r.File), r.Valid))
	}
	// state files sorted by name (the misnamed file is flagged), then changesets in version order
	expected := "a-table.json=true b-tables.json=false v-view.json=true v1_0_9.json=false v1_0_10.json=true"
	if actual := strings.Join(summary, " "); actual != expected {
		t.Errorf("EXPECTED: %s\n  ACTUAL: %s", expected, actual)
	}
}

// every JSON example in the README must be valid - fragments of a table definition are wrapped in a minimal table
func TestReadmeExamplesValid(t *testing.T) {
	reader = osReadWrapper{}
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(string(raw), -1)
	if len(blocks) == 0 {
		t.Fatal("no JSON examples found in the README")
	}
	for i, block := range blocks {
		var doc map[string]any
		if err := json.Unmarshal([]byte(block[1]), &doc); err != nil {
			t.Errorf("README example %d is not valid JSON: %s", i+1, err)
			continue
		}
		var kind FileKind
		switch {
		case doc["deltas"] != nil:
			kind = KindChangeset
		case doc["sql"] != nil:
			kind = KindView
		default:
			kind = KindTable
			if doc["name"] == nil {
				doc["name"] = "example"
			}
			if doc["columns"] == nil {
				doc["columns"] = []any{map[string]any{"name": "id", "datatype": "integer"}}
			}
		}
		normalized, _ := json.Marshal(doc)
		result := ValidateFile(writeTemp(t, "example.json", string(normalized)), kind)
		if !result.Valid {
			firstLine := strings.SplitN(strings.TrimSpace(block[1]), "\n", 3)
			t.Errorf("README example %d (%s, starting %q) is invalid: %+v", i+1, kind, strings.Join(firstLine[:min(2, len(firstLine))], " "), result.Issues)
		}
	}
}
