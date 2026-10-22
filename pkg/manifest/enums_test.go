package manifest

import (
	"errors"
	"skima/pkg/util"
	"testing"

	"github.com/spf13/viper"
)

func TestEnumDdl(t *testing.T) {
	e := Enum{Name: "doc_status", Values: []string{"draft", "in review", "it's done"}}
	if ddl := e.GenerateDdl(); ddl != "create type doc_status as enum ('draft', 'in review', 'it''s done')" {
		t.Errorf("EXPECTED the values quoted as literals  ACTUAL: %s", ddl)
	}
	// a value is added where the enum puts it - after the value before it, or before the next value when first
	cases := map[string]string{
		"draft":     "alter type doc_status add value if not exists 'draft' before 'in review'",
		"in review": "alter type doc_status add value if not exists 'in review' after 'draft'",
		"it's done": "alter type doc_status add value if not exists 'it''s done' after 'in review'",
	}
	for value, expected := range cases {
		if ddl, ok := e.AddValueDdl(value); !ok || ddl != expected {
			t.Errorf("%s: EXPECTED %s  ACTUAL: %s", value, expected, ddl)
		}
	}
	if ddl, _ := (Enum{Name: "one", Values: []string{"only"}}).AddValueDdl("only"); ddl != "alter type one add value if not exists 'only'" {
		t.Errorf("EXPECTED no position for the only value  ACTUAL: %s", ddl)
	}
	if _, ok := e.AddValueDdl("archived"); ok {
		t.Error("EXPECTED a value the enum does not have not to be added")
	}
	if ddl := CommentDdl("enum", "doc_status", "Lifecycle"); ddl != "comment on type doc_status is 'Lifecycle'" {
		t.Errorf("EXPECTED an enum comment to be on the type  ACTUAL: %s", ddl)
	}
}

func TestEnumDelta(t *testing.T) {
	reader = osReadWrapper{}
	viper.Set("workdir", writeManifest(t, map[string]string{
		"state/doc_status-enum.json": `{"name":"doc_status","values":["draft","published","archived"],"comment":"Lifecycle"}`,
	}))
	t.Cleanup(func() { viper.Set("workdir", "") })
	cases := []struct {
		delta    SimpleDelta
		expected string
		missing  bool
	}{
		{SimpleDelta{Object: "enum", Action: "add", Name: "doc_status"}, "create type doc_status as enum ('draft', 'published', 'archived')", false},
		{SimpleDelta{Object: "enum", Action: "drop", Name: "doc_status"}, "drop type if exists doc_status", false},
		{SimpleDelta{Object: "enum", Action: "alter", Name: "doc_status", Value: "archived"}, "alter type doc_status add value if not exists 'archived' after 'published'", false},
		{SimpleDelta{Object: "enum", Action: "alter", Name: "doc_status", Value: "deleted"}, "", true},
		{SimpleDelta{Object: "comment", Action: "add", On: "enum", Name: "doc_status"}, "comment on type doc_status is 'Lifecycle'", false},
	}
	for _, c := range cases {
		ddl, err := c.delta.Ddl()
		var missing *MissingDefinitionError
		if ddl != c.expected || errors.As(err, &missing) != c.missing {
			t.Errorf("%s: EXPECTED %q (missing definition %v)  ACTUAL: %q, %v", c.delta.Describe(), c.expected, c.missing, ddl, err)
		}
	}
	if _, err := (SimpleDelta{Object: "enum", Action: "add", Name: "absent"}).Ddl(); err == nil {
		t.Error("EXPECTED an error for an enum without a state file")
	}
	if d := (SimpleDelta{Object: "enum", Action: "alter", Name: "doc_status", Value: "archived"}); d.Describe() != "alter doc_status (add value 'archived')" {
		t.Errorf("EXPECTED the value in the description  ACTUAL: %s", d.Describe())
	}
}

func TestEnumValidation(t *testing.T) {
	reader = osReadWrapper{}
	workdir := writeManifest(t, map[string]string{
		"state/doc_status-enum.json": `{"name":"doc_status","values":["draft","published"]}`,
		"state/wrong-enum.json":      `{"name":"other","values":["a"]}`,
		"state/dupes-enum.json":      `{"name":"dupes","values":["a","a"],"labels":[]}`,
		"state/empty-enum.json":      `{"name":"empty","values":[]}`,
		"deltas/v1_0_1.json": `{"deltas":[
			{"object":"enum","action":"add","name":"doc_status"},
			{"object":"enum","action":"alter","name":"doc_status","value":"published"},
			{"object":"enum","action":"alter","name":"doc_status","value":"archived"},
			{"object":"enum","action":"add","name":"absent"},
			{"object":"comment","action":"add","on":"enum","name":"doc_status"}
		]}`,
		"deltas/v1_0_2.json": `{"deltas":[
			{"object":"enum","action":"alter","name":"doc_status"},
			{"object":"enum","action":"add","name":"doc_status","value":"draft"},
			{"object":"enum","action":"rename","name":"doc_status"}
		]}`,
	})
	viper.Set("workdir", workdir)
	t.Cleanup(func() { viper.Set("workdir", "") })
	since := util.ExtractVersion("1.0.0")
	results, err := ValidateManifest(workdir, &since)
	if err != nil {
		t.Fatal(err)
	}
	expectIssues(t, results, "doc_status-enum.json")
	expectIssues(t, results, "wrong-enum.json", "error /name: name 'other' does not match the file name - deltas load this file as enum 'wrong'")
	expectIssues(t, results, "dupes-enum.json", "error /values: items at 0 and 1 are equal", "error : additional properties 'labels' not allowed")
	expectIssues(t, results, "empty-enum.json", "error /values: minItems")
	expectIssues(t, results, "v1_0_1.json",
		"error /deltas/2/value: enum 'doc_status' has no value 'archived' in state/doc_status-enum.json",
		"error /deltas/3/name: enum 'absent' has no valid state file (state/absent-enum.json)",
		"error /deltas/4/name: enum 'doc_status' has no comment in state/doc_status-enum.json",
	)
	expectIssues(t, results, "v1_0_2.json",
		"error /deltas/0: missing property 'value'",
		"error /deltas/1: value is only used when the action is alter",
		"error /deltas/2/action: value must be one of 'add', 'drop', 'alter'",
	)
	if kind, ok := KindOf("state/doc_status-enum.json"); !ok || kind != KindEnum {
		t.Errorf("EXPECTED an enum file kind  ACTUAL: %s", kind)
	}
}
