package manifest

import (
	"os"
	"path/filepath"
	"skima/pkg/util"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func writeManifest(t *testing.T, files map[string]string) string {
	t.Helper()
	workdir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(workdir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return workdir
}

// issuesFor returns "severity location: message" lines for a file, by base name
func issuesFor(results []FileValidation, file string) []string {
	var lines []string
	for _, r := range results {
		if filepath.Base(r.File) == file {
			for _, issue := range r.Issues {
				lines = append(lines, issue.Severity+" "+issue.Location+": "+issue.Message)
			}
		}
	}
	return lines
}

func expectIssues(t *testing.T, results []FileValidation, file string, expected ...string) {
	t.Helper()
	actual := issuesFor(results, file)
	if len(actual) != len(expected) {
		t.Errorf("%s - EXPECTED %d issues  ACTUAL %d:\n  %s", file, len(expected), len(actual), strings.Join(actual, "\n  "))
		return
	}
	for i, want := range expected {
		if !strings.HasPrefix(actual[i], want) {
			t.Errorf("%s issue %d - EXPECTED prefix: %s\n  ACTUAL: %s", file, i, want, actual[i])
		}
	}
}

const usersTable = `{"name":"users","columns":[{"name":"id","datatype":"integer"},{"name":"email","datatype":"text"}],
	"primary-key":["id"],
	"indexes":[{"name":"users_email_idx","columns":["email"]}],
	"triggers":[{"name":"users_trg","ddl":"create trigger ..."}],
	"constraints":[{"name":"users_email_unq","type":"unique","columns":["email"]}]}`

const ordersTable = `{"name":"orders","columns":[{"name":"id","datatype":"integer"},{"name":"user_id","datatype":"integer"}],
	"foreign-keys":[{"name":"orders_user_fk","columns":["user_id"],"reference":{"table":"users","columns":["id"]}}]}`

func TestStateReferences(t *testing.T) {
	reader = osReadWrapper{}
	workdir := writeManifest(t, map[string]string{
		"state/users-table.json":  usersTable,
		"state/orders-table.json": ordersTable,
		"state/broken-table.json": `{"name":"broken_tbl","columns":[{"name":"id","datatype":"integer"},{"name":"id","datatype":"text"}],
			"primary-key":["pk_col"],
			"foreign-keys":[
				{"name":"f1","columns":["id"],"reference":{"table":"missing","columns":["id"]}},
				{"name":"f2","columns":["nope"],"reference":{"table":"users","columns":["uid"]}},
				{"name":"f3","columns":["id"],"reference":{"table":"audit.events","columns":["id"]}}],
			"indexes":[{"name":"i","columns":["gone"]},{"name":"j","ddl":"create index j on broken_tbl (id)"}],
			"constraints":[{"name":"c","type":"unique","columns":["gone"]}]}`,
		"state/report-view.json": `{"name":"reports","columns":["id"],"sql":"select id from users"}`,
	})
	results, err := ValidateManifest(workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectIssues(t, results, "users-table.json")
	expectIssues(t, results, "orders-table.json")
	expectIssues(t, results, "broken-table.json",
		"error /name: name 'broken_tbl' does not match the file name - deltas load this file as table 'broken'",
		"error /columns/1/name: column 'id' is defined more than once",
		"error /primary-key/0: primary key column 'pk_col' is not a column of table 'broken_tbl'",
		"error /foreign-keys/0/reference/table: references table 'missing', which has no valid state file",
		"error /foreign-keys/1/columns/0: foreign key column 'nope'",
		"error /foreign-keys/1/reference/columns/0: referenced column 'uid' is not a column of table 'users'",
		"error /indexes/0/columns/0: index column 'gone'",
		"error /constraints/0/columns/0: constraint column 'gone'",
	)
	expectIssues(t, results, "report-view.json",
		"error /name: name 'reports' does not match the file name - deltas load this file as view 'report'")
}

func TestChangesetReferences(t *testing.T) {
	reader = osReadWrapper{}
	files := map[string]string{
		"state/users-table.json":      usersTable,
		"state/orders-table.json":     ordersTable,
		"state/active-view.json":      `{"name":"active","columns":["id"],"sql":"select id from users"}`,
		"state/sql/pre/touch.sql":     "create or replace function touch() ...",
		"state/sql/legacy_fn.sql":     "create or replace function legacy_fn() ...",
		"deltas/scripts/backfill.sql": "update users set email = ''",
		"deltas/v1_0_1.json": `{"deltas":[
			{"object":"table","action":"add","name":"users"},
			{"object":"view","action":"add","name":"active"},
			{"object":"function","action":"add","name":"touch"},
			{"object":"function","action":"add","name":"legacy_fn"},
			{"object":"column","action":"add","name":"email","reference":"users"},
			{"object":"column","action":"alter","name":"email","reference":"users","mods":{"nullable":true}},
			{"object":"column","action":"add","name":"extra","reference":"users","def":{"name":"extra","datatype":"text"}},
			{"object":"index","action":"add","name":"users_email_idx","reference":"users"},
			{"object":"trigger","action":"add","name":"users_trg","reference":"users"},
			{"object":"constraint","action":"add","name":"users_email_unq","reference":"users"},
			{"object":"pk","action":"add","name":"users_pk","reference":"users"},
			{"object":"fk","action":"add","name":"orders_user_fk","reference":"orders"},
			{"object":"table","action":"drop","name":"gone"},
			{"object":"column","action":"rename","name":"old","reference":"gone","newname":"new"},
			{"object":"script","description":"s","script":"backfill.sql"},
			{"object":"table","action":"add","name":"users","pre":{"script":"backfill.sql"}}]}`,
		"deltas/v1_0_2.json": `{"deltas":[
			{"object":"table","action":"add","name":"accounts"},
			{"object":"view","action":"add","name":"inactive"},
			{"object":"function","action":"add","name":"nope"},
			{"object":"column","action":"add","name":"phone","reference":"users"},
			{"object":"column","action":"alter","name":"phone","reference":"users","mods":{"datatype":true}},
			{"object":"index","action":"add","name":"x_idx","reference":"users"},
			{"object":"trigger","action":"add","name":"x_trg","reference":"users"},
			{"object":"constraint","action":"add","name":"x_chk","reference":"users"},
			{"object":"pk","action":"add","name":"orders_pk","reference":"orders"},
			{"object":"fk","action":"add","name":"x_fk","reference":"orders"},
			{"object":"column","action":"add","name":"c","reference":"ghosts"},
			{"object":"script","description":"s","script":"missing.sql"},
			{"object":"table","action":"drop","name":"t","post":{"script":"after.sql"}}]}`,
	}
	workdir := writeManifest(t, files)
	viper.Set("workdir", workdir) // function adds resolve files in the workdir
	t.Cleanup(func() { viper.Set("workdir", "") })

	broken := []string{
		"/deltas/0/name: table 'accounts' has no valid state file",
		"/deltas/1/name: view 'inactive' has no valid state file",
		"/deltas/2/name: function 'nope' has no sql file - expected state/sql/pre/nope.sql or state/sql/nope.sql",
		"/deltas/3/name: table 'users' has no column 'phone' in state/users-table.json",
		"/deltas/4/name: table 'users' has no column 'phone'",
		"/deltas/5/name: table 'users' has no index 'x_idx'",
		"/deltas/6/name: table 'users' has no trigger 'x_trg'",
		"/deltas/7/name: table 'users' has no constraint 'x_chk'",
		"/deltas/8/reference: table 'orders' has no primary-key",
		"/deltas/9/name: table 'orders' has no foreign key 'x_fk'",
		"/deltas/10/reference: table 'ghosts' has no valid state file",
		"/deltas/11/script: script file deltas/scripts/missing.sql does not exist",
		"/deltas/12/post/script: script file deltas/scripts/after.sql does not exist",
	}
	withSeverity := func(severity string) []string {
		lines := make([]string, len(broken))
		for i, line := range broken {
			lines[i] = severity + " " + line
		}
		return lines
	}

	t.Run("warnings by default", func(t *testing.T) {
		results, err := ValidateManifest(workdir, nil)
		if err != nil {
			t.Fatal(err)
		}
		expectIssues(t, results, "v1_0_1.json")
		expectIssues(t, results, "v1_0_2.json", withSeverity(SeverityWarning)...)
		for _, r := range results {
			if !r.Valid {
				t.Errorf("%s should be valid when its only issues are warnings: %+v", r.File, r.Issues)
			}
		}
	})
	t.Run("errors after --since", func(t *testing.T) {
		since := util.ExtractVersion("1.0.1")
		results, err := ValidateManifest(workdir, &since)
		if err != nil {
			t.Fatal(err)
		}
		expectIssues(t, results, "v1_0_2.json", withSeverity(SeverityError)...)
	})
	t.Run("warnings at or before --since", func(t *testing.T) {
		since := util.ExtractVersion("1.0.2")
		results, err := ValidateManifest(workdir, &since)
		if err != nil {
			t.Fatal(err)
		}
		expectIssues(t, results, "v1_0_2.json", withSeverity(SeverityWarning)...)
	})
	t.Run("errors for apply", func(t *testing.T) {
		results, err := ValidateForApply(workdir, []string{filepath.Join(workdir, "deltas", "v1_0_2.json")})
		if err != nil {
			t.Fatal(err)
		}
		expectIssues(t, results, "v1_0_2.json", withSeverity(SeverityError)...)
	})
	t.Run("selected file checked against the workdir state", func(t *testing.T) {
		path := filepath.Join(workdir, "deltas", "v1_0_2.json")
		results, err := ValidateSelected(workdir, []string{path}, map[string]FileKind{path: KindChangeset}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 {
			t.Fatalf("EXPECTED only the selected file  ACTUAL: %d results", len(results))
		}
		expectIssues(t, results, "v1_0_2.json", withSeverity(SeverityWarning)...)
	})
}

func TestChangesetNames(t *testing.T) {
	reader = osReadWrapper{}
	empty := `{"deltas":[]}`
	workdir := writeManifest(t, map[string]string{
		"deltas/v1_0_1.json":      empty,
		"deltas/1_0_1.json":       empty,
		"deltas/v1_0_2-rc_1.json": empty,
		"deltas/v1.0.3.json":      empty,
		"deltas/release-2.json":   empty,
		"proposed.json":           empty,
	})
	results, err := ValidateManifest(workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectIssues(t, results, "v1_0_1.json", "error : has the same version (1.0.1) as 1_0_1.json")
	expectIssues(t, results, "1_0_1.json", "error : has the same version (1.0.1) as v1_0_1.json")
	expectIssues(t, results, "v1_0_2-rc_1.json")
	expectIssues(t, results, "v1.0.3.json", "error : file name is not a version")
	expectIssues(t, results, "release-2.json", "error : file name is not a version")

	// a changeset outside a deltas directory (validated with --type) is not checked for its name
	path := filepath.Join(workdir, "proposed.json")
	selected, err := ValidateSelected(workdir, []string{path}, map[string]FileKind{path: KindChangeset}, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectIssues(t, selected, "proposed.json")
}

func TestFunctionFileLookup(t *testing.T) {
	reader = osReadWrapper{}
	workdir := writeManifest(t, map[string]string{
		"state/sql/pre/both.sql": "from pre",
		"state/sql/both.sql":     "from sql",
		"state/sql/legacy.sql":   "legacy location",
	})
	viper.Set("workdir", workdir)
	t.Cleanup(func() { viper.Set("workdir", "") })
	for name, expected := range map[string]string{"both": "from pre", "legacy": "legacy location"} {
		ddl := SimpleDelta{Object: "function", Action: "add", Name: name}.GenerateDdl()
		if ddl != expected {
			t.Errorf("function %s - EXPECTED: %q  ACTUAL: %q", name, expected, ddl)
		}
	}
}
