package changes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"skima/pkg/manifest"
	"slices"
	"strings"
	"testing"
)

func col(name string, datatype string, nullable bool) manifest.Column {
	return manifest.Column{Name: name, Datatype: datatype, Nullable: nullable}
}

func state(tables ...manifest.Table) State {
	s := State{Tables: map[string]manifest.Table{}, Other: map[string]any{}, Sequences: map[string]manifest.Sequence{}, Sql: map[string]string{}}
	for _, t := range tables {
		s.Tables[t.Name] = t
	}
	return s
}

func clone(t manifest.Table) manifest.Table {
	raw, _ := json.Marshal(t)
	var c manifest.Table
	_ = json.Unmarshal(raw, &c)
	return c
}

var projects = manifest.Table{Name: "projects", Columns: []manifest.Column{col("id", "integer", false), col("name", "text", false)},
	PrimaryKey: []string{"id"}}

var documents = manifest.Table{Name: "documents",
	Columns:     []manifest.Column{col("id", "integer", false), col("project_id", "integer", false), col("file_loc", "text", true)},
	PrimaryKey:  []string{"id"},
	ForeignKeys: []manifest.ForeignKey{{Name: "documents_project_fk", Columns: []string{"project_id"}, Reference: manifest.PrimaryKeyRef{Table: "projects", Columns: []string{"id"}}}},
	Indexes:     []manifest.Index{{Name: "documents_file_loc_idx", Columns: []string{"file_loc"}}},
}

// deltaLines renders deltas compactly, one per line, in changeset order
func deltaLines(deltas []Delta) []string {
	var lines []string
	for _, d := range deltas {
		raw, _ := json.Marshal(d)
		lines = append(lines, string(raw))
	}
	return lines
}

func expectDeltas(t *testing.T, result Result, expected ...string) {
	t.Helper()
	actual := deltaLines(result.Deltas)
	if !slices.Equal(actual, expected) {
		t.Errorf("deltas\n  EXPECTED:\n    %s\n  ACTUAL:\n    %s", strings.Join(expected, "\n    "), strings.Join(actual, "\n    "))
	}
	expectValidChangeset(t, result)
}

// expectValidChangeset checks the generated changeset validates against the changeset schema
func expectValidChangeset(t *testing.T, result Result) {
	t.Helper()
	content, err := Changeset{Description: "test", Deltas: result.Deltas}.Format()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deltas", "v1_0_1.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if v := manifest.ValidateFile(path, manifest.KindChangeset); !v.Valid {
		t.Errorf("the generated changeset is invalid: %+v\n%s", v.Issues, content)
	}
}

func hasWarning(result Result, substring string) bool {
	return slices.ContainsFunc(result.Warnings, func(w string) bool { return strings.Contains(w, substring) })
}

func TestNoChanges(t *testing.T) {
	result := Compare(state(projects, documents), state(projects, documents), Draft{})
	if result.HasChanges() || len(result.Deltas) != 0 {
		t.Errorf("EXPECTED no changes  ACTUAL: %+v", result)
	}
}

func TestTablesAddedAndRemoved(t *testing.T) {
	notes := manifest.Table{Name: "notes", Columns: []manifest.Column{col("id", "integer", false)}, Comment: "notes"}
	result := Compare(state(projects, documents), state(projects, notes), Draft{})
	// documents' own foreign key goes with the table - no separate drop
	expectDeltas(t, result,
		`{"object":"table","action":"add","name":"notes"}`,
		`{"object":"table","action":"drop","name":"documents"}`)
	if !hasWarning(result, "dropping table documents deletes its data") {
		t.Errorf("EXPECTED a destructive warning  ACTUAL: %v", result.Warnings)
	}
}

func TestColumnChanges(t *testing.T) {
	current := clone(documents)
	current.Columns = append(current.Columns, col("status", "text", false), manifest.Column{Name: "notes", Datatype: "text", Nullable: true, Comment: "free text"})
	current.Columns[2].Datatype = "varchar(500)"                                 // file_loc
	current.Columns[1].Nullable = true                                           // project_id
	current.Columns[0].Default, current.Columns[0].Comment = "0", "the document" // id
	result := Compare(state(projects, documents), state(projects, current), Draft{})
	expectDeltas(t, result,
		`{"object":"column","action":"add","name":"status","reference":"documents"}`,
		`{"object":"column","action":"add","name":"notes","reference":"documents"}`,
		`{"object":"column","action":"alter","name":"id","reference":"documents","mods":{"default":true}}`,
		`{"object":"column","action":"alter","name":"project_id","reference":"documents","mods":{"nullable":true}}`,
		`{"object":"column","action":"alter","name":"file_loc","reference":"documents","mods":{"datatype":true}}`,
		`{"object":"comment","action":"add","on":"column","name":"id","reference":"documents"}`)
	for _, warning := range []string{"documents.status is NOT NULL with no default", "changing documents.file_loc to varchar(500) rewrites documents"} {
		if !hasWarning(result, warning) {
			t.Errorf("EXPECTED warning %q  ACTUAL: %v", warning, result.Warnings)
		}
	}
}

func TestRenameHint(t *testing.T) {
	current := clone(documents)
	current.Columns[2].Name = "file_location"
	current.Indexes[0].Columns = []string{"file_location"}
	draft := Draft{}
	draft.RecordRename("documents", "file_loc", "file_location")

	result := Compare(state(projects, documents), state(projects, current), draft)
	// the index on the renamed column is unchanged - postgres renames it with the column
	expectDeltas(t, result, `{"object":"column","action":"rename","name":"file_loc","reference":"documents","newname":"file_location"}`)

	// without the hint the column is dropped and added, with a warning that it looks like a rename
	result = Compare(state(projects, documents), state(projects, current), Draft{})
	expectDeltas(t, result,
		`{"object":"index","action":"drop","name":"documents_file_loc_idx"}`,
		`{"object":"column","action":"add","name":"file_location","reference":"documents"}`,
		`{"object":"index","action":"add","name":"documents_file_loc_idx","reference":"documents"}`,
		`{"object":"column","action":"drop","name":"file_loc","reference":"documents"}`)
	if !hasWarning(result, "documents.file_loc was removed and documents.file_location added with the same datatype - if it is a rename") {
		t.Errorf("EXPECTED a likely rename warning  ACTUAL: %v", result.Warnings)
	}
}

func TestRenameOfReferencedColumn(t *testing.T) {
	// renaming projects.id renames it in documents' foreign key too - no foreign key deltas needed
	currentProjects, currentDocuments := clone(projects), clone(documents)
	currentProjects.Columns[0].Name, currentProjects.PrimaryKey = "project_id", []string{"project_id"}
	currentDocuments.ForeignKeys[0].Reference.Columns = []string{"project_id"}
	draft := Draft{Renames: []RenameHint{{Table: "projects", From: "id", To: "project_id"}}}
	result := Compare(state(projects, documents), state(currentProjects, currentDocuments), draft)
	expectDeltas(t, result, `{"object":"column","action":"rename","name":"id","reference":"projects","newname":"project_id"}`)
}

func TestStaleRenameHintIgnored(t *testing.T) {
	draft := Draft{Renames: []RenameHint{{Table: "documents", From: "file_loc", To: "file_location"}}}
	result := Compare(state(projects, documents), state(projects, documents), draft)
	if result.HasChanges() || !hasWarning(result, "the rename of documents.file_loc to file_location no longer matches the state files - ignored") {
		t.Errorf("EXPECTED no changes and the hint ignored with a warning  ACTUAL: %+v", result)
	}
}

func TestBackfill(t *testing.T) {
	current := clone(documents)
	current.Columns = append(current.Columns, col("status", "text", false), col("owner", "text", false))
	draft := Draft{}
	draft.RecordBackfill(BackfillHint{Table: "documents", Column: "status", Expression: "'draft'"})
	draft.RecordBackfill(BackfillHint{Table: "documents", Column: "owner", Script: "backfill_owner.sql"})
	result := Compare(state(projects, documents), state(projects, current), draft)
	expectDeltas(t, result,
		`{"object":"column","action":"add","name":"status","reference":"documents","def":{"name":"status","datatype":"text","nullable":true}}`,
		`{"object":"script","description":"backfill documents.status","sql":["update documents set status = 'draft' where status is null"]}`,
		`{"object":"column","action":"alter","name":"status","reference":"documents","mods":{"nullable":true}}`,
		`{"object":"column","action":"add","name":"owner","reference":"documents","def":{"name":"owner","datatype":"text","nullable":true}}`,
		`{"object":"script","description":"backfill documents.owner","script":"backfill_owner.sql"}`,
		`{"object":"column","action":"alter","name":"owner","reference":"documents","mods":{"nullable":true}}`)
	if hasWarning(result, "NOT NULL with no default") {
		t.Errorf("EXPECTED no warning for backfilled columns  ACTUAL: %v", result.Warnings)
	}

	// a backfill for a column that doesn't need one is ignored with a warning
	draft = Draft{Backfills: []BackfillHint{{Table: "documents", Column: "file_loc", Expression: "''"}}}
	if result = Compare(state(projects, documents), state(projects, documents), draft); !hasWarning(result, "the backfill for documents.file_loc is not needed") {
		t.Errorf("EXPECTED an unused backfill warning  ACTUAL: %v", result.Warnings)
	}
}

func TestTableObjects(t *testing.T) {
	current := clone(documents)
	current.PrimaryKey = []string{"id", "project_id"}
	current.ForeignKeys[0].DeleteRule = "cascade"
	current.Indexes = nil
	current.Constraints = []manifest.Constraint{{Name: "documents_file_loc_unq", Type: "unique", Columns: []string{"file_loc"}}}
	current.Triggers = []manifest.Trigger{{Name: "documents_trg", Ddl: "create trigger documents_trg before update on documents for each row execute function f()"}}
	current.Comment = "documents"
	result := Compare(state(projects, documents), state(projects, current), Draft{})
	expectDeltas(t, result,
		`{"object":"pk","action":"drop","name":"documents_pk","reference":"documents"}`,
		`{"object":"fk","action":"drop","name":"documents_project_fk","reference":"documents"}`,
		`{"object":"index","action":"drop","name":"documents_file_loc_idx"}`,
		`{"object":"pk","action":"add","name":"documents_pk","reference":"documents"}`,
		`{"object":"constraint","action":"add","name":"documents_file_loc_unq","reference":"documents"}`,
		`{"object":"trigger","action":"add","name":"documents_trg","reference":"documents"}`,
		`{"object":"fk","action":"add","name":"documents_project_fk","reference":"documents"}`,
		`{"object":"comment","action":"add","on":"table","name":"documents"}`)
}

func TestUnsupportedChanges(t *testing.T) {
	base, current := state(projects), state(projects)
	base.Other["view active"] = map[string]any{"name": "active", "sql": "select 1"}
	current.Other["view active"] = map[string]any{"name": "active", "sql": "select 2"}
	current.Other["enum color"] = map[string]any{"name": "color"}
	base.Sql[filepath.Join("state", "sql", "pre", "f.sql")] = "create function f() ..."
	current.Sequences["seq"] = manifest.Sequence{Name: "seq"}
	withIdentity := clone(projects)
	withIdentity.Columns = append(withIdentity.Columns, manifest.Column{Name: "n", Datatype: "integer", Primarykey: true, Generate: "always"})
	current.Tables["projects"] = withIdentity
	result := Compare(base, current, Draft{})
	var unsupported []string
	for _, u := range result.Unsupported {
		unsupported = append(unsupported, u.Object+" "+u.Target+" "+u.Action)
	}
	expected := []string{"column projects.n added", "enum color added", "view active changed", "sequence seq added", "sql file " + filepath.Join("state", "sql", "pre", "f.sql") + " removed"}
	if !slices.Equal(unsupported, expected) {
		t.Errorf("unsupported\n  EXPECTED: %v\n  ACTUAL:   %v", expected, unsupported)
	}
	if len(result.Deltas) != 0 || !result.HasChanges() {
		t.Errorf("EXPECTED changes but no deltas  ACTUAL: %+v", result)
	}
}

func TestDraftRenames(t *testing.T) {
	var d Draft
	d.RecordBackfill(BackfillHint{Table: "t", Column: "a", Expression: "1"})
	d.RecordRename("t", "a", "b")
	d.RecordRename("t", "b", "c") // chains to a -> c
	if !slices.Equal(d.Renames, []RenameHint{{Table: "t", From: "a", To: "c"}}) || d.Backfills[0].Column != "c" {
		t.Errorf("EXPECTED a single rename a -> c with the backfill following it  ACTUAL: %+v", d)
	}
	d.RecordRename("t", "c", "a") // renamed back
	if len(d.Renames) != 0 {
		t.Errorf("EXPECTED renaming back to remove the hint  ACTUAL: %+v", d.Renames)
	}
	d.RecordRename("t", "x", "y")
	d.ForgetColumn("t", "y")
	d.ForgetColumn("t", "a")
	if len(d.Renames) != 0 || len(d.Backfills) != 0 {
		t.Errorf("EXPECTED dropped columns to lose their hints  ACTUAL: %+v", d)
	}
}

func TestDraftSaveLoad(t *testing.T) {
	workdir := t.TempDir()
	draft := Draft{Renames: []RenameHint{{Table: "t", From: "a", To: "b"}}}
	if err := draft.Save(workdir); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadDraft(workdir)
	if err != nil || !slices.Equal(loaded.Renames, draft.Renames) {
		t.Errorf("EXPECTED the draft back  ACTUAL: %+v %v", loaded, err)
	}
	if ignore, err := os.ReadFile(filepath.Join(workdir, ".skima", ".gitignore")); err != nil || string(ignore) != "*\n" {
		t.Errorf("EXPECTED .skima/.gitignore ignoring everything  ACTUAL: %q %v", ignore, err)
	}
	if err = (Draft{}).Save(workdir); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(DraftPath(workdir)); !os.IsNotExist(err) {
		t.Error("EXPECTED saving an empty draft to remove the file")
	}
}

func TestViewWarnings(t *testing.T) {
	current := clone(projects)
	current.Columns[1].Datatype = "varchar(200)" // name
	cur := state(current)
	cur.Other["view project_names"] = map[string]any{"name": "project_names", "sql": "select p.name from projects p"}
	cur.Other["view unrelated"] = map[string]any{"name": "unrelated", "sql": "select name from other"}
	result := Compare(state(projects), cur, Draft{})
	if !hasWarning(result, "changing the datatype of projects.name fails while view project_names uses it") || hasWarning(result, "unrelated") {
		t.Errorf("EXPECTED a warning for project_names only  ACTUAL: %v", result.Warnings)
	}
}
