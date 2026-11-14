package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// parseState parses a state file of any kind into its manifest type
func parseState(t *testing.T, path string) any {
	t.Helper()
	kind, _ := KindOf(path)
	var parsed any
	var err error
	switch kind {
	case KindTable:
		parsed, err = ParseTableByPath(path)
	case KindView:
		parsed, err = ParseViewByPath(path)
	case KindEnum:
		parsed, err = ParseEnumByPath(path)
	case KindSequence:
		parsed, err = ParseSequenceByPath(path)
	case KindMatview:
		parsed, err = ParseMatviewByPath(path)
	default:
		t.Fatalf("%s is not a state file", path)
	}
	if err != nil {
		t.Fatalf("parsing %s: %s", path, err)
	}
	return parsed
}

// Formatting must never change what a file means: every fixture state file formats to a valid file that parses to
// the same object, and formatting that again changes nothing
func TestFormatStateFileRoundTrip(t *testing.T) {
	reader = osReadWrapper{}
	fixtures, err := StateFiles("../plan/testdata/manifest")
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < 5 {
		t.Fatalf("EXPECTED fixtures of every state kind  ACTUAL: %v", fixtures)
	}
	for _, path := range fixtures {
		t.Run(filepath.Base(path), func(t *testing.T) {
			formatted, _, err := FormatStateFile(path)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), filepath.Base(path))
			if err := os.WriteFile(out, formatted, 0o644); err != nil {
				t.Fatal(err)
			}
			kind, _ := KindOf(out)
			if result := ValidateFile(out, kind); !result.Valid {
				t.Fatalf("formatted file is invalid: %+v\n%s", result.Issues, formatted)
			}
			if original, again := parseState(t, path), parseState(t, out); !reflect.DeepEqual(original, again) {
				t.Errorf("formatting changed the content\n  EXPECTED: %+v\n  ACTUAL:   %+v", original, again)
			}
			reformatted, changed, err := FormatStateFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if changed || string(reformatted) != string(formatted) {
				t.Errorf("formatting is not stable - a formatted file changed when formatted again:\n%s", reformatted)
			}
		})
	}
}

func TestFormatStateFileCanonical(t *testing.T) {
	reader = osReadWrapper{}
	// keys out of order, 4-space indent, nullable left out, empty optional arrays, and SQL with < > &
	messy := `{
    "comment": "orders & returns",
    "constraints": [{"condition": "total >= 0 AND total < 1000000", "type": "check", "name": "orders_total_chk"}],
    "indexes": [],
    "columns": [
        {"datatype": "integer", "name": "id", "nullable": false},
        {"comment": "free text", "name": "note", "datatype": "text"},
        {"name": "created_at", "default": "now()", "datatype": "timestamptz", "dfltfunc": true, "nullable": false}
    ],
    "primary-key": ["id"],
    "$schema": "https://example.com/table.schema.json",
    "name": "orders"
}`
	expected := `{
  "$schema": "https://example.com/table.schema.json",
  "name": "orders",
  "columns": [
    {
      "name": "id",
      "datatype": "integer",
      "nullable": false
    },
    {
      "name": "note",
      "datatype": "text",
      "nullable": false,
      "comment": "free text"
    },
    {
      "name": "created_at",
      "datatype": "timestamptz",
      "nullable": false,
      "default": "now()",
      "dfltfunc": true
    }
  ],
  "primary-key": [
    "id"
  ],
  "constraints": [
    {
      "name": "orders_total_chk",
      "type": "check",
      "condition": "total >= 0 AND total < 1000000"
    }
  ],
  "comment": "orders & returns"
}
`
	path := filepath.Join(t.TempDir(), "orders-table.json")
	if err := os.WriteFile(path, []byte(messy), 0o644); err != nil {
		t.Fatal(err)
	}
	formatted, changed, err := FormatStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("EXPECTED the messy file to be reported as changed")
	}
	if string(formatted) != expected {
		t.Errorf("EXPECTED:\n%s\nACTUAL:\n%s", expected, formatted)
	}
}

func TestFormatStateFileRefusesInvalidOrNonStateFiles(t *testing.T) {
	reader = osReadWrapper{}
	dir := t.TempDir()
	invalid := filepath.Join(dir, "users-table.json")
	// a misspelled property would be dropped by parsing, so the file must not be formatted
	if err := os.WriteFile(invalid, []byte(`{"name":"users","columns":[{"name":"id","datatype":"integer","nullabel":true}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	formatted, _, err := FormatStateFile(invalid)
	var invalidErr *InvalidFileError
	if !errors.As(err, &invalidErr) || formatted != nil {
		t.Errorf("invalid file - EXPECTED an InvalidFileError and no content  ACTUAL: %v, %q", err, formatted)
	} else if len(invalidErr.Validation.Issues) == 0 {
		t.Error("EXPECTED the validation issues on the error")
	}

	changeset := filepath.Join(dir, "deltas", "v1_0_1.json")
	if err := os.MkdirAll(filepath.Dir(changeset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changeset, []byte(`{"deltas":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := FormatStateFile(changeset); err == nil {
		t.Error("changeset - EXPECTED an error, changesets are not formatted")
	}
}

// Every field of every state kind survives formatting - the fixtures don't use them all
func TestFormatEveryField(t *testing.T) {
	reader = osReadWrapper{}
	noData := false
	idx := []Index{{Name: "i1", Columns: []string{"a", "b"}, Unique: true}, {Name: "i2", Ddl: "create index i2 on t (lower(a))"}}
	cases := []struct {
		file   string
		object any
		format func(string) ([]byte, error)
	}{
		{"t-table.json", Table{Name: "t",
			Columns: []Column{
				{Name: "id", Datatype: "integer", Nullable: false, Primarykey: true, Generate: "by default", Comment: "key"},
				{Name: "a", Datatype: "text", Nullable: true, Default: "x", Comment: "a's comment"},
				{Name: "b", Datatype: "timestamptz", Default: "now()", Dfltfunc: true}},
			ForeignKeys: []ForeignKey{{Name: "fk", Columns: []string{"a"}, Reference: PrimaryKeyRef{Table: "p", Columns: []string{"id"}}, DeleteRule: "cascade", UpdateRule: "set null"}},
			Indexes:     idx,
			Triggers:    []Trigger{{Name: "trg", Ddl: "create trigger trg before update on t for each row execute function f()"}},
			Constraints: []Constraint{{Name: "u", Type: "unique", Columns: []string{"a"}}, {Name: "c", Type: "check", Condition: "id > 0 and a <> ''"}},
			Comment:     "the table"},
			nil},
		{"v-view.json", View{Name: "v", Columns: []string{"id", "a"}, Sql: "select id, a from t where id < 10", Comment: "the view"}, nil},
		{"e-enum.json", Enum{Name: "e", Values: []string{"low", "high"}, Comment: "the enum"}, nil},
		{"s-sequence.json", Sequence{Name: "s", Datatype: "bigint", Start: int64p(0), Increment: int64p(-1), Minvalue: int64p(-100),
			Maxvalue: int64p(0), Cache: int64p(5), Cycle: true, Ownedby: "t.id", Comment: "the sequence"}, nil},
		{"m-matview.json", Matview{Name: "m", Columns: []string{"a"}, Sql: "select a from t", Data: &noData, Indexes: idx, Comment: "the matview"}, nil},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			var formatted []byte
			var err error
			switch o := c.object.(type) {
			case Table:
				formatted, err = FormatTable(o, "")
			case View:
				formatted, err = FormatView(o, "")
			case Enum:
				formatted, err = FormatEnum(o, "")
			case Sequence:
				formatted, err = FormatSequence(o, "")
			case Matview:
				formatted, err = FormatMatview(o, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), c.file)
			if err := os.WriteFile(path, formatted, 0o644); err != nil {
				t.Fatal(err)
			}
			if parsed := parseState(t, path); !reflect.DeepEqual(parsed, c.object) {
				t.Errorf("a field was lost\n  EXPECTED: %+v\n  ACTUAL:   %+v\n%s", c.object, parsed, formatted)
			}
		})
	}
}
