package editor

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"skima/pkg/manifest"
	"slices"
	"strings"
	"testing"
)

// fixture copies the plan fixture manifest's state files into a temporary working directory
func fixture(t *testing.T) (*Editor, string) {
	t.Helper()
	workdir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workdir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob("../plan/testdata/manifest/state/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixture state files: %v", err)
	}
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(workdir, "state", filepath.Base(path)), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New(workdir), workdir
}

// snapshot reads every state file, to check what an edit changed
func snapshot(t *testing.T, workdir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	paths, _ := filepath.Glob(filepath.Join(workdir, "state", "*"))
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(path)] = string(content)
	}
	return files
}

// changedFiles lists the files that differ between two snapshots
func changedFiles(before, after map[string]string) []string {
	var changed []string
	for name, content := range after {
		if before[name] != content {
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			changed = append(changed, name+" (deleted)")
		}
	}
	slices.Sort(changed)
	return changed
}

func etag(t *testing.T, e *Editor, table string) string {
	t.Helper()
	_, tag, err := e.Table(table)
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

func mustTable(t *testing.T, e *Editor, name string) manifest.Table {
	t.Helper()
	table, _, err := e.Table(name)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func hasWarning(result Result, substring string) bool {
	return slices.ContainsFunc(result.Warnings, func(w string) bool { return strings.Contains(w, substring) })
}

func TestRead(t *testing.T) {
	e, _ := fixture(t)
	names, err := e.Names(manifest.KindTable)
	if err != nil || !reflect.DeepEqual(names, []string{"documents", "projects", "tags"}) {
		t.Errorf("table names - EXPECTED documents, projects, tags  ACTUAL: %v %v", names, err)
	}
	doc, err := e.Get(manifest.KindSequence, "tag_seq")
	if err != nil || doc.ETag != ETag(doc.Content) || doc.Path != filepath.Join("state", "tag_seq-sequence.json") {
		t.Errorf("get sequence: %+v %v", doc, err)
	}
	if _, err = e.Get(manifest.KindView, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing view - EXPECTED ErrNotFound  ACTUAL: %v", err)
	}
	if _, err = e.Get(manifest.KindTable, "../../etc/passwd"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("path in a name - EXPECTED ErrInvalidName  ACTUAL: %v", err)
	}
}

func TestEditWritesOnlyChangedFilesCanonically(t *testing.T) {
	e, workdir := fixture(t)
	before := snapshot(t, workdir)
	result, err := e.AddColumn("projects", etag(t, e, "projects"), manifest.Column{Name: "archived", Datatype: "boolean", Default: "false", Nullable: false})
	if err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, workdir)
	if changed := changedFiles(before, after); !reflect.DeepEqual(changed, []string{"projects-table.json"}) {
		t.Errorf("EXPECTED only projects-table.json to change  ACTUAL: %v", changed)
	}
	canonical, err := manifest.FormatTable(*result.Table, "")
	if err != nil {
		t.Fatal(err)
	}
	if after["projects-table.json"] != string(canonical) {
		t.Errorf("written file is not in the canonical format:\n%s", after["projects-table.json"])
	}
	if result.ETag != ETag([]byte(after["projects-table.json"])) || !reflect.DeepEqual(result.Written, []string{filepath.Join("state", "projects-table.json")}) {
		t.Errorf("result - ETag %s, written %v", result.ETag, result.Written)
	}
	if last := result.Table.Columns[len(result.Table.Columns)-1]; last.Name != "archived" {
		t.Errorf("EXPECTED the column added at the end  ACTUAL: %+v", result.Table.Columns)
	}
	if temps, _ := filepath.Glob(filepath.Join(workdir, "state", ".*")); len(temps) != 0 {
		t.Errorf("EXPECTED no temporary files left  ACTUAL: %v", temps)
	}
}

func TestStaleETag(t *testing.T) {
	e, workdir := fixture(t)
	stale := etag(t, e, "projects")
	if _, err := e.SetTableComment("projects", stale, "first"); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, workdir)
	if _, err := e.SetTableComment("projects", stale, "second"); !errors.Is(err, ErrStale) {
		t.Errorf("EXPECTED ErrStale  ACTUAL: %v", err)
	}
	if changed := changedFiles(before, snapshot(t, workdir)); len(changed) != 0 {
		t.Errorf("a rejected edit changed files: %v", changed)
	}
}

func TestCreateAndDropTable(t *testing.T) {
	e, workdir := fixture(t)
	table := manifest.Table{Name: "notes", Columns: []manifest.Column{{Name: "id", Datatype: "integer"}, {Name: "body", Datatype: "text", Nullable: true}},
		PrimaryKey: []string{"id"}, Comment: "notes"}
	result, err := e.CreateTable(table)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Written, []string{filepath.Join("state", "notes-table.json")}) || !reflect.DeepEqual(mustTable(t, e, "notes"), table) {
		t.Errorf("create - written %v, table %+v", result.Written, mustTable(t, e, "notes"))
	}
	if _, err = e.CreateTable(table); !errors.Is(err, ErrExists) {
		t.Errorf("create again - EXPECTED ErrExists  ACTUAL: %v", err)
	}
	if _, err = e.CreateTable(manifest.Table{Name: "Bad-Name", Columns: table.Columns}); !errors.Is(err, ErrInvalidName) {
		t.Errorf("bad name - EXPECTED ErrInvalidName  ACTUAL: %v", err)
	}
	result, err = e.DropTable("notes", etag(t, e, "notes"), false)
	if err != nil || result.Table != nil || !reflect.DeepEqual(result.Deleted, []string{filepath.Join("state", "notes-table.json")}) {
		t.Errorf("drop - %+v %v", result, err)
	}
	if _, err = os.Stat(filepath.Join(workdir, "state", "notes-table.json")); !os.IsNotExist(err) {
		t.Error("EXPECTED the table file deleted")
	}
}

func TestDropTableReferenced(t *testing.T) {
	e, workdir := fixture(t)
	before := snapshot(t, workdir)
	_, err := e.DropTable("documents", etag(t, e, "documents"), false)
	var referenced *ReferencedError
	if !errors.As(err, &referenced) || !reflect.DeepEqual(referenced.References, []string{"foreign key tags.tags_document_fk"}) {
		t.Fatalf("EXPECTED a ReferencedError for tags.tags_document_fk  ACTUAL: %v", err)
	}
	if changed := changedFiles(before, snapshot(t, workdir)); len(changed) != 0 {
		t.Errorf("a rejected drop changed files: %v", changed)
	}
	result, err := e.DropTable("documents", etag(t, e, "documents"), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(mustTable(t, e, "tags").ForeignKeys) != 0 || !hasWarning(result, "dropped foreign key tags.tags_document_fk") {
		t.Errorf("cascade - EXPECTED the tags foreign key dropped with a warning  ACTUAL: %+v", result.Warnings)
	}
	if !hasWarning(result, "project_documents-view.json's sql mentions 'documents'") {
		t.Errorf("EXPECTED a warning about the view that uses the table  ACTUAL: %v", result.Warnings)
	}
}

func TestRenameColumnCascades(t *testing.T) {
	e, workdir := fixture(t)
	before := snapshot(t, workdir)
	result, err := e.RenameColumn("documents", etag(t, e, "documents"), "id", "document_id")
	if err != nil {
		t.Fatal(err)
	}
	documents, tags := mustTable(t, e, "documents"), mustTable(t, e, "tags")
	if documents.Columns[0].Name != "document_id" || !reflect.DeepEqual(documents.PrimaryKey, []string{"document_id"}) {
		t.Errorf("EXPECTED the column and primary key renamed  ACTUAL: %+v %v", documents.Columns[0], documents.PrimaryKey)
	}
	if !reflect.DeepEqual(tags.ForeignKeys[0].Reference.Columns, []string{"document_id"}) {
		t.Errorf("EXPECTED the referencing foreign key on tags renamed  ACTUAL: %+v", tags.ForeignKeys[0])
	}
	if changed := changedFiles(before, snapshot(t, workdir)); !reflect.DeepEqual(changed, []string{"documents-table.json", "tags-table.json"}) {
		t.Errorf("EXPECTED documents and tags rewritten  ACTUAL: %v", changed)
	}
	if !reflect.DeepEqual(result.Renames, []Rename{{Table: "documents", From: "id", To: "document_id"}}) {
		t.Errorf("EXPECTED the rename reported  ACTUAL: %+v", result.Renames)
	}
	if !hasWarning(result, "project_documents-view.json's sql mentions 'documents.id'") {
		t.Errorf("EXPECTED a warning about the view  ACTUAL: %v", result.Warnings)
	}

	// a column that owns a sequence takes the sequence with it
	if _, err = e.RenameColumn("tags", etag(t, e, "tags"), "seq_no", "position"); err != nil {
		t.Fatal(err)
	}
	seq, _ := e.Get(manifest.KindSequence, "tag_seq")
	if !strings.Contains(string(seq.Content), `"ownedby": "tags.position"`) {
		t.Errorf("EXPECTED the sequence's ownedby renamed  ACTUAL:\n%s", seq.Content)
	}
	if _, err = e.RenameColumn("tags", etag(t, e, "tags"), "label", "color"); !errors.Is(err, ErrExists) {
		t.Errorf("rename onto an existing column - EXPECTED ErrExists  ACTUAL: %v", err)
	}
}

func TestDropColumnReferenced(t *testing.T) {
	e, workdir := fixture(t)
	before := snapshot(t, workdir)
	_, err := e.DropColumn("documents", etag(t, e, "documents"), "id", false)
	var referenced *ReferencedError
	if !errors.As(err, &referenced) || !reflect.DeepEqual(referenced.References, []string{"the primary key", "foreign key tags.tags_document_fk"}) {
		t.Fatalf("EXPECTED a ReferencedError for the primary key and tags' foreign key  ACTUAL: %v", err)
	}
	if changed := changedFiles(before, snapshot(t, workdir)); len(changed) != 0 {
		t.Errorf("a rejected drop changed files: %v", changed)
	}

	result, err := e.DropColumn("documents", etag(t, e, "documents"), "document_type", true)
	if err != nil {
		t.Fatal(err)
	}
	documents := mustTable(t, e, "documents")
	if columnIndex(&documents, "document_type") >= 0 || len(documents.Indexes) != 0 {
		t.Errorf("cascade - EXPECTED the column and its index dropped  ACTUAL: %+v", documents)
	}
	if !hasWarning(result, "dropped index documents_type_idx") || !hasWarning(result, "constraint documents_document_type_check's condition mentions 'document_type'") {
		t.Errorf("EXPECTED warnings for the dropped index and the check that mentions the column  ACTUAL: %v", result.Warnings)
	}

	// a column that owns a sequence: the sequence goes with it on cascade
	if _, err = e.DropColumn("tags", etag(t, e, "tags"), "seq_no", false); !errors.As(err, &referenced) {
		t.Errorf("EXPECTED the owned sequence to block the drop  ACTUAL: %v", err)
	}
	if result, err = e.DropColumn("tags", etag(t, e, "tags"), "seq_no", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Deleted, []string{filepath.Join("state", "tag_seq-sequence.json")}) {
		t.Errorf("EXPECTED the sequence file deleted  ACTUAL: %+v", result)
	}
}

func TestInvalidEditRejected(t *testing.T) {
	e, workdir := fixture(t)
	before := snapshot(t, workdir)
	_, err := e.SetPrimaryKey("projects", etag(t, e, "projects"), []string{"nope"})
	var invalid *InvalidEditError
	if !errors.As(err, &invalid) || len(invalid.Issues) != 1 || invalid.Issues[0].File != filepath.Join("state", "projects-table.json") ||
		!strings.Contains(invalid.Issues[0].Message, "'nope' is not a column") {
		t.Fatalf("EXPECTED an InvalidEditError for the primary key column  ACTUAL: %v", err)
	}
	if changed := changedFiles(before, snapshot(t, workdir)); len(changed) != 0 {
		t.Errorf("a rejected edit changed files: %v", changed)
	}
}

func TestExistingProblemsDoNotBlockEdits(t *testing.T) {
	e, workdir := fixture(t)
	// break the view by hand - an unrelated edit must still be allowed, with the problem as a warning
	broken := `{"name":"elsewhere","columns":["n"],"sql":"select 1"}`
	if err := os.WriteFile(filepath.Join(workdir, "state", "common_types-view.json"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := e.SetTableComment("tags", etag(t, e, "tags"), "labels on documents")
	if err != nil {
		t.Fatalf("EXPECTED the edit to be allowed  ACTUAL: %v", err)
	}
	if !hasWarning(result, "existing problem in "+filepath.Join("state", "common_types-view.json")) {
		t.Errorf("EXPECTED the existing problem as a warning  ACTUAL: %v", result.Warnings)
	}
}

func TestPutAndDropTableObjects(t *testing.T) {
	e, _ := fixture(t)
	if _, err := e.PutIndex("tags", etag(t, e, "tags"), manifest.Index{Name: "tags_label_idx", Columns: []string{"label"}}); err != nil {
		t.Fatal(err)
	}
	// putting the same name replaces it in place
	if _, err := e.PutIndex("tags", etag(t, e, "tags"), manifest.Index{Name: "tags_label_idx", Columns: []string{"label"}, Unique: true}); err != nil {
		t.Fatal(err)
	}
	if idx := mustTable(t, e, "tags").Indexes; len(idx) != 1 || !idx[0].Unique {
		t.Errorf("EXPECTED one unique index  ACTUAL: %+v", idx)
	}
	if _, err := e.PutConstraint("tags", etag(t, e, "tags"), manifest.Constraint{Name: "tags_label_chk", Type: "check", Condition: "label <> ''"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutTrigger("tags", etag(t, e, "tags"), manifest.Trigger{Name: "tags_trg", Ddl: "create trigger tags_trg before update on tags for each row execute function update_last_modified_at()"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutForeignKey("documents", etag(t, e, "documents"), manifest.ForeignKey{Name: "documents_project_fk", Columns: []string{"project_id"},
		Reference: manifest.PrimaryKeyRef{Table: "projects", Columns: []string{"id"}}, DeleteRule: "restrict"}); err != nil {
		t.Fatal(err)
	}
	if fks := mustTable(t, e, "documents").ForeignKeys; len(fks) != 1 || fks[0].DeleteRule != "restrict" {
		t.Errorf("EXPECTED the foreign key replaced  ACTUAL: %+v", fks)
	}
	for _, drop := range []func() (Result, error){
		func() (Result, error) { return e.DropIndex("tags", etag(t, e, "tags"), "tags_label_idx") },
		func() (Result, error) { return e.DropConstraint("tags", etag(t, e, "tags"), "tags_label_chk") },
		func() (Result, error) { return e.DropTrigger("tags", etag(t, e, "tags"), "tags_trg") },
	} {
		if _, err := drop(); err != nil {
			t.Fatal(err)
		}
	}
	if tags := mustTable(t, e, "tags"); len(tags.Indexes)+len(tags.Constraints)+len(tags.Triggers) != 0 {
		t.Errorf("EXPECTED everything dropped  ACTUAL: %+v", tags)
	}
	if _, err := e.DropIndex("tags", etag(t, e, "tags"), "tags_label_idx"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropping a missing index - EXPECTED ErrNotFound  ACTUAL: %v", err)
	}
}

func TestPrimaryKeyAndAlterColumn(t *testing.T) {
	e, _ := fixture(t)
	_, err := e.DropPrimaryKey("projects", etag(t, e, "projects"), false)
	var referenced *ReferencedError
	if !errors.As(err, &referenced) || !reflect.DeepEqual(referenced.References, []string{"foreign key documents.documents_project_fk"}) {
		t.Fatalf("EXPECTED documents' foreign key to block dropping the primary key  ACTUAL: %v", err)
	}
	datatype, nullable, comment := "bigint", true, "the key"
	if _, err = e.AlterColumn("tags", etag(t, e, "tags"), "label", ColumnChanges{Datatype: &datatype, Nullable: &nullable, Comment: &comment}); err != nil {
		t.Fatal(err)
	}
	tags := mustTable(t, e, "tags")
	label := tags.Columns[columnIndex(&tags, "label")]
	if label.Datatype != "bigint" || !label.Nullable || label.Comment != "the key" {
		t.Errorf("EXPECTED the column altered  ACTUAL: %+v", label)
	}
	if _, err = e.SetPrimaryKey("tags", etag(t, e, "tags"), []string{"id"}); err != nil {
		t.Fatal(err)
	}
	if pk := mustTable(t, e, "tags").PrimaryKey; !reflect.DeepEqual(pk, []string{"id"}) {
		t.Errorf("EXPECTED the primary key set  ACTUAL: %v", pk)
	}
	if _, err = e.AlterColumn("tags", etag(t, e, "tags"), "nope", ColumnChanges{Comment: &comment}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing column - EXPECTED ErrNotFound  ACTUAL: %v", err)
	}
}

func TestSchemaPropertyKept(t *testing.T) {
	e, workdir := fixture(t)
	path := filepath.Join(workdir, "state", "projects-table.json")
	content, _ := os.ReadFile(path)
	withSchema := strings.Replace(string(content), "{", `{"$schema": "https://example.com/table.schema.json",`, 1)
	if err := os.WriteFile(path, []byte(withSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetTableComment("projects", etag(t, e, "projects"), "kept"); err != nil {
		t.Fatal(err)
	}
	content, _ = os.ReadFile(path)
	if !strings.HasPrefix(string(content), "{\n  \"$schema\": \"https://example.com/table.schema.json\",") {
		t.Errorf("EXPECTED $schema kept as the first property  ACTUAL:\n%s", content)
	}
}
