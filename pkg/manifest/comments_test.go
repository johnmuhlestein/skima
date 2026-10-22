package manifest

import (
	"errors"
	"skima/pkg/util"
	"testing"

	"github.com/spf13/viper"
)

func TestCommentDdl(t *testing.T) {
	cases := []struct{ on, target, comment, expected string }{
		{"table", "docs", "Uploaded documents", "comment on table docs is 'Uploaded documents'"},
		{"column", "docs.title", "The document's title", "comment on column docs.title is 'The document''s title'"},
		// backslashes are ordinary characters in a standard string literal
		{"view", "doc_counts", `C:\temp`, `comment on view doc_counts is 'C:\temp'`},
		{"table", "docs", "", "comment on table docs is null"},
	}
	for _, c := range cases {
		if actual := CommentDdl(c.on, c.target, c.comment); actual != c.expected {
			t.Errorf("EXPECTED %s  ACTUAL: %s", c.expected, actual)
		}
	}
}

const commentedDocs = `{"name":"docs","comment":"Uploaded documents","columns":[
	{"name":"id","datatype":"uuid"},
	{"name":"title","datatype":"text","comment":"The document's title"},
	{"name":"body","datatype":"text"}]}`

func TestCommentDelta(t *testing.T) {
	reader = osReadWrapper{}
	viper.Set("workdir", writeManifest(t, map[string]string{
		"state/docs-table.json":   commentedDocs,
		"state/summary-view.json": `{"name":"summary","columns":["n"],"sql":"select count(*) from docs","comment":"How many"}`,
		"state/plain-view.json":   `{"name":"plain","columns":["n"],"sql":"select 1"}`,
	}))
	t.Cleanup(func() { viper.Set("workdir", "") })

	cases := []struct {
		delta    SimpleDelta
		expected string
		missing  bool
	}{
		{SimpleDelta{Object: "comment", Action: "add", On: "table", Name: "docs"}, "comment on table docs is 'Uploaded documents'", false},
		{SimpleDelta{Object: "comment", Action: "add", On: "column", Name: "title", Reference: "docs"}, "comment on column docs.title is 'The document''s title'", false},
		{SimpleDelta{Object: "comment", Action: "add", On: "view", Name: "summary"}, "comment on view summary is 'How many'", false},
		{SimpleDelta{Object: "comment", Action: "drop", On: "column", Name: "body", Reference: "docs"}, "comment on column docs.body is null", false},
		// no comment in the state file - nothing to add
		{SimpleDelta{Object: "comment", Action: "add", On: "column", Name: "body", Reference: "docs"}, "", true},
		{SimpleDelta{Object: "comment", Action: "add", On: "view", Name: "plain"}, "", true},
		{SimpleDelta{Object: "comment", Action: "add", On: "column", Name: "nope", Reference: "docs"}, "", true},
	}
	for _, c := range cases {
		ddl, err := c.delta.Ddl()
		var missing *MissingDefinitionError
		if ddl != c.expected || errors.As(err, &missing) != c.missing {
			t.Errorf("%s: EXPECTED %q (missing definition %v)  ACTUAL: %q, %v", c.delta.Describe(), c.expected, c.missing, ddl, err)
		}
	}
	if d := (SimpleDelta{Object: "comment", Action: "add", On: "column", Name: "title", Reference: "docs"}); d.Describe() != "add comment on column docs.title" {
		t.Errorf("EXPECTED a description naming what is commented on  ACTUAL: %s", d.Describe())
	}
}

func TestCommentDeltaValidation(t *testing.T) {
	reader = osReadWrapper{}
	workdir := writeManifest(t, map[string]string{
		"state/docs-table.json":   commentedDocs,
		"state/summary-view.json": `{"name":"summary","columns":["n"],"sql":"select count(*) from docs"}`,
		"deltas/v1_0_1.json": `{"deltas":[
			{"object":"comment","action":"add","on":"table","name":"docs"},
			{"object":"comment","action":"add","on":"column","name":"title","reference":"docs"},
			{"object":"comment","action":"drop","on":"column","name":"body","reference":"docs"},
			{"object":"comment","action":"add","on":"column","name":"body","reference":"docs"},
			{"object":"comment","action":"add","on":"view","name":"summary"},
			{"object":"comment","action":"add","on":"table","name":"absent"}
		]}`,
		"deltas/v1_0_2.json": `{"deltas":[
			{"object":"comment","action":"add","on":"column","name":"title"},
			{"object":"comment","action":"add","on":"table","name":"docs","reference":"docs"},
			{"object":"comment","action":"set","on":"index","name":"docs_idx"}
		]}`,
	})
	viper.Set("workdir", workdir)
	t.Cleanup(func() { viper.Set("workdir", "") })
	since := util.ExtractVersion("1.0.0")
	results, err := ValidateManifest(workdir, &since)
	if err != nil {
		t.Fatal(err)
	}
	expectIssues(t, results, "v1_0_1.json",
		"error /deltas/3/name: column 'docs.body' has no comment in state/docs-table.json",
		"error /deltas/4/name: view 'summary' has no comment in state/summary-view.json",
		"error /deltas/5/name: table 'absent' has no valid state file",
	)
	// a column comment needs its table, others must not have one, and only add/drop on tables, columns and views
	expectIssues(t, results, "v1_0_2.json",
		"error /deltas/0: missing property 'reference'",
		"error /deltas/1: reference is only used for a column comment",
		"error /deltas/2/action: value must be one of 'add', 'drop'",
		"error /deltas/2/on: value must be one of 'table', 'column', 'view'",
	)
}
