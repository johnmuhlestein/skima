package drift

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// go test ./pkg/drift -update rewrites the golden files from the current output
var update = flag.Bool("update", false, "rewrite the golden files")

// documents is the schema the state files describe in these tests - each test changes a copy of it
func documents() Schema {
	s := NewSchema()
	t := s.Table("documents")
	t.Columns["id"] = Column{Type: "uuid", NotNull: true}
	t.Columns["document_type"] = Column{Type: "character varying", NotNull: true}
	t.Columns["priority"] = Column{Type: "integer", Default: "0"}
	t.Constraints["documents_pk"] = "PRIMARY KEY (id)"
	t.Constraints["documents_type_check"] = "CHECK (((document_type)::text = ANY ((ARRAY['A'::character varying])::text[])))"
	t.Indexes["documents_type_idx"] = "CREATE INDEX documents_type_idx ON documents USING btree (document_type)"
	t.Triggers["documents_trg"] = "CREATE TRIGGER documents_trg BEFORE UPDATE ON documents FOR EACH ROW EXECUTE FUNCTION touch()"
	tags := s.Table("tags")
	tags.Columns["id"] = Column{Type: "integer", NotNull: true, Identity: "always"}
	s.Views["document_types"] = View{Definition: " SELECT DISTINCT document_type\n   FROM documents;", Comment: "Distinct types"}
	return s
}

func describe(findings []Finding) []string {
	var described []string
	for _, f := range findings {
		d := fmt.Sprintf("%s %s %s", f.Change, f.Object, f.Name)
		if f.Attribute != "" {
			d += " " + f.Attribute
		}
		described = append(described, d)
	}
	return described
}

func TestNoDrift(t *testing.T) {
	findings := Compare(documents(), documents())
	if findings == nil || len(findings) != 0 {
		t.Errorf("EXPECTED an empty (not nil) list of findings  ACTUAL: %#v", findings)
	}
}

func TestTables(t *testing.T) {
	actual := documents()
	delete(actual.Tables, "tags")
	actual.Table("legacy").Columns["id"] = Column{Type: "integer"}
	// a missing table is one finding - not one per column
	expected := []string{"extra table legacy", "missing table tags"}
	if got := describe(Compare(documents(), actual)); strings.Join(got, "|") != strings.Join(expected, "|") {
		t.Errorf("EXPECTED %v  ACTUAL: %v", expected, got)
	}
}

func TestColumns(t *testing.T) {
	actual := documents()
	docs := actual.Tables["documents"]
	delete(docs.Columns, "priority")
	docs.Columns["legacy_ref"] = Column{Type: "text"}
	docs.Columns["document_type"] = Column{Type: "character varying(20)", Default: "'A'::character varying"}
	tags := actual.Tables["tags"]
	tags.Columns["id"] = Column{Type: "bigint", NotNull: true, Identity: "by default"}

	findings := Compare(documents(), actual)
	expected := []string{
		"changed column documents.document_type default",
		"changed column documents.document_type nullable",
		"changed column documents.document_type type",
		"extra column documents.legacy_ref",
		"missing column documents.priority",
		"changed column tags.id identity",
		"changed column tags.id type",
	}
	if got := describe(findings); strings.Join(got, "|") != strings.Join(expected, "|") {
		t.Fatalf("EXPECTED %v\nACTUAL: %v", expected, got)
	}
	byName := map[string]Finding{}
	for _, f := range findings {
		byName[f.Name+" "+f.Attribute] = f
	}
	checks := []struct{ key, expected, actual string }{
		{"documents.document_type nullable", "not null", "null"},
		{"documents.document_type default", "none", "'A'::character varying"},
		{"documents.priority ", "integer default 0", ""},
		{"documents.legacy_ref ", "", "text"},
		{"tags.id identity", "always", "by default"},
	}
	for _, c := range checks {
		if f := byName[c.key]; f.Expected != c.expected || f.Actual != c.actual {
			t.Errorf("%s: EXPECTED %q -> %q  ACTUAL: %q -> %q", c.key, c.expected, c.actual, f.Expected, f.Actual)
		}
	}
}

func TestDefinitions(t *testing.T) {
	actual := documents()
	docs := actual.Tables["documents"]
	docs.Constraints["documents_type_check"] = "CHECK (((document_type)::text = ANY ((ARRAY['A'::character varying, 'B'::character varying])::text[])))"
	delete(docs.Constraints, "documents_pk")
	docs.Constraints["documents_pkey"] = "PRIMARY KEY (id)"
	delete(docs.Indexes, "documents_type_idx")
	docs.Indexes["documents_priority_idx"] = "CREATE INDEX documents_priority_idx ON documents USING btree (priority)"
	docs.Triggers["documents_trg"] = "CREATE TRIGGER documents_trg AFTER UPDATE ON documents FOR EACH ROW EXECUTE FUNCTION touch()"
	actual.Views["document_types"] = View{Definition: " SELECT document_type\n   FROM documents;", Comment: "Distinct types"}
	actual.Views["old_report"] = View{Definition: " SELECT 1;"}

	expected := []string{
		// grouped by table or view name - document_types sorts before documents
		"changed view document_types",
		// a renamed constraint is a missing one and an extra one
		"missing constraint documents.documents_pk",
		"extra constraint documents.documents_pkey",
		"changed constraint documents.documents_type_check",
		"extra index documents.documents_priority_idx",
		"missing index documents.documents_type_idx",
		"changed trigger documents.documents_trg",
		"extra view old_report",
	}
	if got := describe(Compare(documents(), actual)); strings.Join(got, "|") != strings.Join(expected, "|") {
		t.Errorf("EXPECTED %v\nACTUAL: %v", expected, got)
	}
}

func TestIgnore(t *testing.T) {
	actual := documents()
	actual.Table("legacy_audit").Columns["id"] = Column{Type: "integer"}
	actual.Table("legacy_users")
	actual.Tables["documents"].Columns["legacy_ref"] = Column{Type: "text"}
	actual.Tables["documents"].Indexes["documents_label_idx"] = "CREATE INDEX documents_label_idx ON documents USING btree (id)"
	delete(actual.Tables["tags"].Columns, "id")

	report := Report{Findings: Compare(documents(), actual)}
	// a table pattern covers the table and everything on it, a qualified pattern one object
	if err := report.Ignore([]string{"legacy_*", "documents.*_idx", "tags"}); err != nil {
		t.Fatal(err)
	}
	if got := describe(report.Findings); strings.Join(got, "|") != "extra column documents.legacy_ref" || report.Ignored != 4 {
		t.Errorf("EXPECTED only documents.legacy_ref left, 4 ignored  ACTUAL: %v, %d ignored", got, report.Ignored)
	}
	if err := report.Ignore([]string{"documents.[a"}); err == nil || !strings.Contains(err.Error(), "invalid ignore pattern") {
		t.Errorf("EXPECTED an invalid pattern error  ACTUAL: %v", err)
	}
}

func golden(t *testing.T, name string, actual []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, actual, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("unable to read %s (run go test ./pkg/drift -update to create it): %s", path, err)
	}
	if !bytes.Equal(expected, actual) {
		t.Errorf("output does not match %s (run go test ./pkg/drift -update if the change is intended)\n--- expected\n%s\n--- actual\n%s", path, expected, actual)
	}
}

func TestRender(t *testing.T) {
	actual := documents()
	docs := actual.Tables["documents"]
	docs.Columns["legacy_ref"] = Column{Type: "text"}
	docs.Columns["priority"] = Column{Type: "integer", Default: "5"}
	delete(docs.Indexes, "documents_type_idx")
	actual.Table("legacy_audit")
	actual.Views["document_types"] = View{Definition: " SELECT document_type\n   FROM documents;", Comment: "Distinct types"}
	report := Report{Schema: "app1", Deployed: "1.0.3", Pending: []string{"1.0.4", "1.0.5"}, Findings: Compare(documents(), actual)}
	if err := report.Ignore([]string{"legacy_*"}); err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	Render(&text, report)
	golden(t, "drift.txt", text.Bytes())
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "drift.json", append(out, '\n'))

	var none bytes.Buffer
	Render(&none, Report{Schema: "app1", Deployed: "1.0.3", Pending: []string{"1.0.4"}, Findings: Compare(documents(), documents())})
	golden(t, "no-drift.txt", none.Bytes())
	out, err = json.Marshal(Report{Findings: Compare(documents(), documents())})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"findings":[]`) {
		t.Errorf("EXPECTED an empty findings list in the JSON  ACTUAL: %s", out)
	}
}

func TestComments(t *testing.T) {
	expected := documents()
	expected.Tables["documents"].Comment = "Uploaded documents"
	title := expected.Tables["documents"].Columns["priority"]
	title.Comment = "Higher is sooner"
	expected.Tables["documents"].Columns["priority"] = title

	actual := documents()
	actual.Tables["tags"].Comment = "Added by hand"
	view := actual.Views["document_types"]
	view.Comment = "It's changed"
	actual.Views["document_types"] = view

	findings := Compare(expected, actual)
	want := []string{
		"changed view document_types comment",
		"changed table documents comment",
		"changed column documents.priority comment",
		"changed table tags comment",
	}
	if got := describe(findings); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("EXPECTED %v\nACTUAL: %v", want, got)
	}
	checks := map[string][2]string{
		"document_types":     {"Distinct types", "It's changed"},
		"documents":          {"Uploaded documents", "none"},
		"documents.priority": {"Higher is sooner", "none"},
		"tags":               {"none", "Added by hand"},
	}
	for _, f := range findings {
		if c := checks[f.Name]; f.Expected != c[0] || f.Actual != c[1] {
			t.Errorf("%s: EXPECTED %q -> %q  ACTUAL: %q -> %q", f.Name, c[0], c[1], f.Expected, f.Actual)
		}
	}
}

func TestEnums(t *testing.T) {
	expected := documents()
	expected.Enums["doc_status"] = Enum{Values: []string{"draft", "published"}, Comment: "Lifecycle"}
	expected.Enums["priority"] = Enum{Values: []string{"low", "high"}}
	expected.Enums["region"] = Enum{Values: []string{"east", "west"}}

	actual := documents()
	// a value added by hand, and an enum whose values are in another order - postgres sorts by that order
	actual.Enums["doc_status"] = Enum{Values: []string{"draft", "published", "archived"}}
	actual.Enums["priority"] = Enum{Values: []string{"high", "low"}}
	actual.Enums["legacy_kind"] = Enum{Values: []string{"a"}}

	findings := Compare(expected, actual)
	want := []string{
		"changed enum doc_status",
		"changed enum doc_status comment",
		"extra enum legacy_kind",
		"changed enum priority",
		"missing enum region",
	}
	if got := describe(findings); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("EXPECTED %v\nACTUAL: %v", want, got)
	}
	if f := findings[0]; f.Expected != "draft, published" || f.Actual != "draft, published, archived" {
		t.Errorf("EXPECTED the values listed in order  ACTUAL: %q -> %q", f.Expected, f.Actual)
	}
	report := Report{Findings: findings}
	if err := report.Ignore([]string{"legacy_*"}); err != nil || len(report.Findings) != 4 {
		t.Errorf("EXPECTED an ignore pattern to match an enum's name  ACTUAL: %v, %d findings", err, len(report.Findings))
	}
}

func TestSequences(t *testing.T) {
	seq := Sequence{Type: "bigint", Start: 1000, Increment: 5, Min: 1, Max: 9223372036854775807, Cache: 10, OwnedBy: "tags.seq_no", Comment: "Numbers tags"}
	expected := documents()
	expected.Sequences["tag_seq"] = seq
	expected.Sequences["report_no"] = Sequence{Type: "bigint", Start: 1, Increment: 1, Min: 1, Max: 100, Cache: 1}

	actual := documents()
	changed := seq
	changed.Increment, changed.Cycle, changed.OwnedBy, changed.Comment = 7, true, "", ""
	actual.Sequences["tag_seq"] = changed
	// an unowned sequence nobody defined is extra - one a column owns (e.g. a serial column's) belongs to the column
	actual.Sequences["stray_seq"] = Sequence{Type: "bigint", Start: 1, Increment: 1, Min: 1, Max: 9223372036854775807, Cache: 1}
	actual.Sequences["projects_legacy_no_seq"] = Sequence{Type: "integer", Start: 1, Increment: 1, Min: 1, Max: 2147483647, Cache: 1, OwnedBy: "projects.legacy_no"}

	findings := Compare(expected, actual)
	want := []string{
		"missing sequence report_no",
		"extra sequence stray_seq",
		"changed sequence tag_seq comment",
		"changed sequence tag_seq cycle",
		"changed sequence tag_seq increment",
		"changed sequence tag_seq owned by",
	}
	if got := describe(findings); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("EXPECTED %v\nACTUAL: %v", want, got)
	}
	byName := map[string]Finding{}
	for _, f := range findings {
		byName[f.Name+" "+f.Attribute] = f
	}
	if f := byName["tag_seq owned by"]; f.Expected != "tags.seq_no" || f.Actual != "none" {
		t.Errorf("EXPECTED owned by tags.seq_no -> none  ACTUAL: %q -> %q", f.Expected, f.Actual)
	}
	if f := byName["report_no "]; f.Expected != "bigint start 1 increment 1 minvalue 1 maxvalue 100 cache 1" {
		t.Errorf("EXPECTED the missing sequence described  ACTUAL: %q", f.Expected)
	}
}
