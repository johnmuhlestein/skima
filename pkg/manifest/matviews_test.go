package manifest

import (
	"skima/pkg/util"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestMatviewDdl(t *testing.T) {
	noData := false
	mv := Matview{Name: "doc_counts", Columns: []string{"status", "n"}, Sql: "select status, count(*) from docs group by status"}
	empty := mv
	empty.Data = &noData
	cases := map[string]string{
		mv.GenerateDdl():    "create materialized view if not exists doc_counts (status,n) as select status, count(*) from docs group by status with data",
		empty.GenerateDdl(): "create materialized view if not exists doc_counts (status,n) as select status, count(*) from docs group by status with no data",
		// a shadow build stands in a scratch view for the definition and an empty table for its indexes and references
		strings.Join(mv.ShadowDdl(), "; "): "create or replace view skima_matview_doc_counts (status,n) as select status, count(*) from docs group by status; " +
			"create table doc_counts as select * from skima_matview_doc_counts with no data",
		CommentDdl("matview", "doc_counts", "Counts"): "comment on materialized view doc_counts is 'Counts'",
	}
	for actual, expected := range cases {
		if actual != expected {
			t.Errorf("EXPECTED %s\n  ACTUAL: %s", expected, actual)
		}
	}
	if mv.UniqueIndex() {
		t.Error("EXPECTED no unique index")
	}
	mv.Indexes = []Index{{Name: "i", Columns: []string{"n"}}, {Name: "u", Columns: []string{"status"}, Unique: true}}
	if !mv.UniqueIndex() {
		t.Error("EXPECTED a unique index")
	}
}

func TestMatviewDelta(t *testing.T) {
	reader = osReadWrapper{}
	viper.Set("workdir", writeManifest(t, map[string]string{
		"state/doc_counts-matview.json": `{"name":"doc_counts","columns":["status","n"],"sql":"select status, count(*) from docs group by status",
			"indexes":[{"name":"doc_counts_status_idx","columns":["status"],"unique":true}],"comment":"Counts"}`,
	}))
	t.Cleanup(func() { viper.Set("workdir", "") })
	cases := []struct {
		delta    SimpleDelta
		expected string
	}{
		{SimpleDelta{Object: "matview", Action: "add", Name: "doc_counts"}, "create materialized view if not exists doc_counts (status,n) as select status, count(*) from docs group by status with data"},
		{SimpleDelta{Object: "matview", Action: "drop", Name: "doc_counts"}, "drop materialized view if exists doc_counts"},
		{SimpleDelta{Object: "matview", Action: "refresh", Name: "doc_counts"}, "refresh materialized view doc_counts"},
		{SimpleDelta{Object: "matview", Action: "refresh", Name: "doc_counts", Concurrently: true}, "refresh materialized view concurrently doc_counts"},
		// an index delta finds a materialized view's index in its state file
		{SimpleDelta{Object: "index", Action: "add", Name: "doc_counts_status_idx", Reference: "doc_counts"}, "create unique index if not exists doc_counts_status_idx on doc_counts (status)"},
		{SimpleDelta{Object: "comment", Action: "add", On: "matview", Name: "doc_counts"}, "comment on materialized view doc_counts is 'Counts'"},
	}
	for _, c := range cases {
		if ddl, err := c.delta.Ddl(); err != nil || ddl != c.expected {
			t.Errorf("%s: EXPECTED %s  ACTUAL: %s, %v", c.delta.Describe(), c.expected, ddl, err)
		}
	}
	if d := (SimpleDelta{Object: "matview", Action: "refresh", Name: "doc_counts", Concurrently: true}); d.Describe() != "refresh doc_counts (concurrently)" {
		t.Errorf("EXPECTED concurrently in the description  ACTUAL: %s", d.Describe())
	}
	if _, err := (SimpleDelta{Object: "index", Action: "add", Name: "x", Reference: "absent"}).Ddl(); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Errorf("EXPECTED an error naming the missing table  ACTUAL: %v", err)
	}
}

func TestMatviewValidation(t *testing.T) {
	reader = osReadWrapper{}
	workdir := writeManifest(t, map[string]string{
		"state/doc_counts-matview.json": `{"name":"doc_counts","columns":["status","n"],"sql":"select 1, 2",
			"indexes":[{"name":"doc_counts_status_idx","columns":["status"],"unique":true}]}`,
		"state/plain-matview.json":   `{"name":"plain","columns":["n"],"sql":"select 1","data":false,"indexes":[{"name":"plain_idx","columns":["missing"]}]}`,
		"state/renamed-matview.json": `{"name":"other","columns":["n"],"sql":"select 1"}`,
		"state/bad-matview.json":     `{"name":"bad","sql":"select 1","refresh":true}`,
		"deltas/v1_0_1.json": `{"deltas":[
			{"object":"matview","action":"add","name":"doc_counts"},
			{"object":"matview","action":"refresh","name":"doc_counts","concurrently":true},
			{"object":"matview","action":"refresh","name":"plain","concurrently":true},
			{"object":"matview","action":"refresh","name":"absent"},
			{"object":"matview","action":"add","name":"absent"},
			{"object":"index","action":"add","name":"doc_counts_status_idx","reference":"doc_counts"},
			{"object":"index","action":"add","name":"nope_idx","reference":"doc_counts"},
			{"object":"comment","action":"add","on":"matview","name":"doc_counts"}
		]}`,
		"deltas/v1_0_2.json": `{"deltas":[{"object":"matview","action":"add","name":"doc_counts","concurrently":true}]}`,
	})
	viper.Set("workdir", workdir)
	t.Cleanup(func() { viper.Set("workdir", "") })
	since := util.ExtractVersion("1.0.0")
	results, err := ValidateManifest(workdir, &since)
	if err != nil {
		t.Fatal(err)
	}
	expectIssues(t, results, "doc_counts-matview.json")
	expectIssues(t, results, "plain-matview.json", "error /indexes/0/columns/0: index column 'missing' is not a column of materialized view 'plain'")
	expectIssues(t, results, "renamed-matview.json", "error /name: name 'other' does not match the file name - deltas load this file as materialized view 'renamed'")
	expectIssues(t, results, "bad-matview.json", "error : additional properties 'refresh' not allowed", "error : missing property 'columns'")
	// a plain refresh does not read the state file - a refresh concurrently checks for the unique index it needs
	expectIssues(t, results, "v1_0_1.json",
		"error /deltas/2/concurrently: a refresh concurrently needs a unique index on materialized view 'plain'",
		"error /deltas/4/name: materialized view 'absent' has no valid state file (state/absent-matview.json)",
		"error /deltas/6/name: materialized view 'doc_counts' has no index 'nope_idx' in state/doc_counts-matview.json",
		"error /deltas/7/name: matview 'doc_counts' has no comment in state/doc_counts-matview.json",
	)
	expectIssues(t, results, "v1_0_2.json", "error /deltas/0: concurrently is only used when the action is refresh")
	if kind, ok := KindOf("state/doc_counts-matview.json"); !ok || kind != KindMatview {
		t.Errorf("EXPECTED a materialized view file kind  ACTUAL: %s", kind)
	}
}
