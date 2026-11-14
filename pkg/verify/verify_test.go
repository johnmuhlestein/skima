package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"skima/pkg/changes"
	"skima/pkg/editor"
	"skima/pkg/manifest"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// database skips the test unless a postgres is configured: SKIMA_VERIFY_DATABASE=1 and the usual SKM_DBCONN_* settings
// (connstring, port, database, schema, username, password). Verify only needs the TEMP privilege
func database(t *testing.T) {
	t.Helper()
	if os.Getenv("SKIMA_VERIFY_DATABASE") != "1" {
		t.Skip("set SKIMA_VERIFY_DATABASE=1 and SKM_DBCONN_* to verify changesets against postgres")
	}
	viper.SetEnvPrefix("skm")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
}

// fixture commits a copy of the plan fixture manifest to a new git repository - the baseline
func fixture(t *testing.T) (string, changes.Baseline) {
	t.Helper()
	root := t.TempDir()
	workdir := filepath.Join(root, "schema")
	if err := os.CopyFS(workdir, os.DirFS("../plan/testdata/manifest")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "schema at 1.0.3"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	baseline, err := changes.ResolveBaseline(workdir, "")
	if err != nil {
		t.Fatal(err)
	}
	return workdir, baseline
}

// edits makes edits through the editor, recording the hints the API would
type edits struct {
	t     *testing.T
	ed    *editor.Editor
	draft changes.Draft
}

func (e *edits) etag(table string) string {
	_, tag, err := e.ed.Table(table)
	if err != nil {
		e.t.Fatal(err)
	}
	return tag
}

func (e *edits) must(result editor.Result, err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range result.Renames {
		e.draft.RecordRename(r.Table, r.From, r.To)
	}
}

func str(s string) *string { return &s }
func boolean(b bool) *bool { return &b }

func expectVerified(t *testing.T, workdir string, baseline changes.Baseline, draft changes.Draft) {
	t.Helper()
	report, err := Run(workdir, baseline, draft)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.Deltas == 0 {
		t.Errorf("EXPECTED the generated changeset (%d deltas) to verify\n  apply error: %s\n  findings: %+v", report.Deltas, report.ApplyError, report.Findings)
	}
}

func TestVerifyColumnChanges(t *testing.T) {
	database(t)
	workdir, baseline := fixture(t)
	e := &edits{t: t, ed: editor.New(workdir)}
	e.must(e.ed.RenameColumn("documents", e.etag("documents"), "file_location", "storage_path"))
	e.must(e.ed.AddColumn("documents", e.etag("documents"), manifest.Column{Name: "status", Datatype: "text"}))
	e.draft.RecordBackfill(changes.BackfillHint{Table: "documents", Column: "status", Expression: "'draft'"})
	e.must(e.ed.AddColumn("documents", e.etag("documents"), manifest.Column{Name: "notes", Datatype: "text", Nullable: true, Comment: "free text"}))
	e.must(e.ed.AlterColumn("tags", e.etag("tags"), "label", editor.ColumnChanges{Datatype: str("varchar(100)"), Comment: str("shown on the tag")}))
	e.must(e.ed.DropColumn("tags", e.etag("tags"), "color", false))
	e.must(e.ed.AlterColumn("projects", e.etag("projects"), "name", editor.ColumnChanges{Default: str("unnamed")}))
	e.must(e.ed.SetTableComment("projects", e.etag("projects"), "projects documents belong to"))
	expectVerified(t, workdir, baseline, e.draft)
}

func TestVerifyTables(t *testing.T) {
	database(t)
	workdir, baseline := fixture(t)
	e := &edits{t: t, ed: editor.New(workdir)}
	e.must(e.ed.CreateTable(manifest.Table{Name: "audit_log", Comment: "who changed what",
		Columns:     []manifest.Column{{Name: "id", Datatype: "integer"}, {Name: "project_id", Datatype: "uuid", Nullable: true}, {Name: "msg", Datatype: "text", Nullable: true, Comment: "what changed"}},
		PrimaryKey:  []string{"id"},
		ForeignKeys: []manifest.ForeignKey{{Name: "audit_log_project_fk", Columns: []string{"project_id"}, Reference: manifest.PrimaryKeyRef{Table: "projects", Columns: []string{"id"}}}},
		Indexes:     []manifest.Index{{Name: "audit_log_project_idx", Columns: []string{"project_id"}}}}))
	e.must(e.ed.DropTable("tags", e.etag("tags"), true)) // cascade: its sequence goes too
	expectVerified(t, workdir, baseline, e.draft)
}

func TestVerifyKeysAndObjects(t *testing.T) {
	database(t)
	workdir, baseline := fixture(t)
	e := &edits{t: t, ed: editor.New(workdir)}
	documents, _, _ := e.ed.Table("documents")
	fk := documents.ForeignKeys[0]
	fk.DeleteRule = "restrict"
	e.must(e.ed.PutForeignKey("documents", e.etag("documents"), fk))
	e.must(e.ed.PutConstraint("documents", e.etag("documents"), manifest.Constraint{Name: "documents_document_type_check", Type: "check",
		Condition: "document_type in ('STRUCTURED','UNSTRUCTURED','MIXED')"}))
	e.must(e.ed.DropIndex("documents", e.etag("documents"), "documents_type_idx"))
	e.must(e.ed.PutIndex("tags", e.etag("tags"), manifest.Index{Name: "tags_label_idx", Columns: []string{"label"}, Unique: true}))
	e.must(e.ed.PutConstraint("tags", e.etag("tags"), manifest.Constraint{Name: "tags_label_chk", Type: "check", Condition: "label <> ''"}))
	e.must(e.ed.PutTrigger("tags", e.etag("tags"), manifest.Trigger{Name: "tags_touch_trg",
		Ddl: "create trigger tags_touch_trg before update on tags for each row execute function update_last_modified_at()"}))
	// documents' primary key changes while tags' foreign key references it - kept valid by a unique constraint on id
	e.must(e.ed.PutConstraint("documents", e.etag("documents"), manifest.Constraint{Name: "documents_id_unq", Type: "unique", Columns: []string{"id"}}))
	e.must(e.ed.SetPrimaryKey("documents", e.etag("documents"), []string{"id", "project_id"}))
	e.must(e.ed.AlterColumn("tags", e.etag("tags"), "document_id", editor.ColumnChanges{Nullable: boolean(true)}))
	expectVerified(t, workdir, baseline, e.draft)
}

// verify must catch a changeset that doesn't bring the baseline to the current state - otherwise it proves nothing
func TestVerifyCatchesWrongChangesets(t *testing.T) {
	database(t)
	workdir, baseline := fixture(t)
	e := &edits{t: t, ed: editor.New(workdir)}
	e.must(e.ed.AddColumn("documents", e.etag("documents"), manifest.Column{Name: "notes", Datatype: "text", Nullable: true}))
	e.must(e.ed.PutIndex("tags", e.etag("tags"), manifest.Index{Name: "tags_label_idx", Columns: []string{"label"}}))
	diff, err := changes.Diff(workdir, baseline, e.draft)
	if err != nil || len(diff.Deltas) != 2 {
		t.Fatalf("EXPECTED 2 generated deltas  ACTUAL: %+v %v", diff.Deltas, err)
	}

	// a delta left out: the index is missing
	report, err := Deltas(workdir, baseline, diff.Deltas[:1])
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || len(report.Findings) != 1 || report.Findings[0].Change != "missing" || report.Findings[0].Name != "tags.tags_label_idx" {
		t.Errorf("EXPECTED the missing index found  ACTUAL: %+v", report)
	}

	// a delta that fails to apply
	broken := append(diff.Deltas, changes.Delta{Object: "script", Description: "broken", Sql: []string{"alter table no_such_table add column x integer"}})
	report, err = Deltas(workdir, baseline, broken)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || !strings.Contains(report.ApplyError, "no_such_table") {
		t.Errorf("EXPECTED the failing statement reported  ACTUAL: %+v", report)
	}
}
