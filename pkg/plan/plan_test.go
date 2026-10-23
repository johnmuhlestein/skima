package plan

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"skima/pkg/manifest"
	"skima/pkg/util"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// go test ./pkg/plan -update rewrites the golden files from the current output
var update = flag.Bool("update", false, "rewrite the golden files")

const fixture = "testdata/manifest"

func useManifest(t *testing.T, workdir string) {
	t.Helper()
	viper.Set("workdir", workdir)
	viper.Set("dbconn.schema", "app1")
	t.Cleanup(func() {
		viper.Set("workdir", "")
		viper.Set("dbconn.schema", "")
	})
}

// golden compares the output with testdata/golden/<name>, or rewrites it with -update
func golden(t *testing.T, name string, actual []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
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
		t.Fatalf("unable to read %s (run go test ./pkg/plan -update to create it): %s", path, err)
	}
	if !bytes.Equal(expected, actual) {
		t.Errorf("output does not match %s (run go test ./pkg/plan -update if the change is intended)\n--- expected\n%s\n--- actual\n%s", path, expected, actual)
	}
}

func renderText(p Plan, opts RenderOptions) []byte {
	var buf bytes.Buffer
	Render(&buf, p, opts)
	return buf.Bytes()
}

func renderJSON(t *testing.T, p Plan) []byte {
	t.Helper()
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func TestStatePlan(t *testing.T) {
	useManifest(t, fixture)
	p, err := ForState(true)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "state.txt", renderText(p, RenderOptions{}))
	golden(t, "state.json", renderJSON(t, p))
	golden(t, "state-scripts.txt", renderText(p, RenderOptions{ShowScripts: true}))

	// the build order apply uses: pre sql, tables (each with its indexes, triggers and constraints), fks, views, post sql
	var kinds []string
	for _, step := range p.Units[0].Steps {
		if len(kinds) == 0 || kinds[len(kinds)-1] != step.Kind {
			kinds = append(kinds, step.Kind)
		}
	}
	expected := "sql enum comment sequence comment table index constraint comment table index trigger comment table sequence fk view comment matview index comment sql"
	if actual := strings.Join(kinds, " "); actual != expected {
		t.Errorf("EXPECTED step order %q  ACTUAL: %q", expected, actual)
	}
}

func TestStatePlanWithoutTransaction(t *testing.T) {
	useManifest(t, fixture)
	p, err := ForState(false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Units[0].Transactional || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "without a transaction") {
		t.Errorf("EXPECTED a non-transactional state build with a warning  ACTUAL: transactional=%v warnings=%v", p.Units[0].Transactional, p.Warnings)
	}
}

func TestChangesetPlan(t *testing.T) {
	useManifest(t, fixture)
	p, err := ForChangesets(util.ExtractVersion("1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "changesets.txt", renderText(p, RenderOptions{}))
	golden(t, "changesets.json", renderJSON(t, p))
	golden(t, "changesets-scripts.txt", renderText(p, RenderOptions{ShowScripts: true}))

	if p.From != "1.0.0" || p.To != "1.0.3" || len(p.Units) != 3 {
		t.Errorf("EXPECTED 3 changesets from 1.0.0 to 1.0.3  ACTUAL: %d from %s to %s", len(p.Units), p.From, p.To)
	}
	// the added table's fk is added after every other delta of the changeset
	first := p.Units[0].Steps
	if last := first[len(first)-1]; last.Kind != KindFk || !last.Deferred {
		t.Errorf("EXPECTED the deferred fk last in changeset 1.0.1  ACTUAL: %+v", last)
	}
	// every statement of a sql delta is planned, not just the first
	for _, step := range p.Units[1].Steps {
		if step.Describe == "seed the default tags" && len(step.Sql) != 2 {
			t.Errorf("EXPECTED both statements of the sql delta  ACTUAL: %v", step.Sql)
		}
	}
}

func TestChangesetPlanFromLaterVersion(t *testing.T) {
	useManifest(t, fixture)
	p, err := ForChangesets(util.ExtractVersion("1.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Units) != 1 || p.Units[0].Version != "1.0.3" || p.Units[0].Transactional {
		t.Errorf("EXPECTED only the non-transactional changeset 1.0.3  ACTUAL: %+v", p.Units)
	}
}

func TestUpToDatePlan(t *testing.T) {
	useManifest(t, fixture)
	p, err := ForChangesets(util.ExtractVersion("1.0.3"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Pending() || p.Statements() != 0 {
		t.Errorf("EXPECTED no pending changes  ACTUAL: %+v", p)
	}
	golden(t, "up-to-date.txt", renderText(p, RenderOptions{}))
}

// writeManifest writes the files (path relative to the manifest -> content) to a new working directory
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

const notesTable = `{"name":"notes","columns":[{"name":"id","datatype":"uuid","nullable":false}],"primary-key":["id"]}`

// a delta whose definition is not in the state files generates no DDL - apply skips it and does not run its post hook
func TestSkippedDelta(t *testing.T) {
	useManifest(t, writeManifest(t, map[string]string{
		"state/notes-table.json": notesTable,
		"deltas/v1_0_1.json": `{"deltas":[{"object":"column","action":"add","name":"body","reference":"notes",
			"pre":{"sql":["select 1"]},"post":{"sql":["select 2"]}}]}`,
	}))
	p, err := ForChangesets(util.ExtractVersion("1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	steps := p.Units[0].Steps
	if len(steps) != 2 || steps[0].Kind != KindPreHook || !steps[1].Skipped {
		t.Fatalf("EXPECTED the pre hook then the skipped delta, without the post hook  ACTUAL: %+v", steps)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "did not have a column definition matching body") {
		t.Errorf("EXPECTED a warning about the missing column  ACTUAL: %v", p.Warnings)
	}
	if p.Statements() != 1 {
		t.Errorf("EXPECTED the skipped delta not to be counted  ACTUAL: %d statements", p.Statements())
	}
	if text := string(renderText(p, RenderOptions{})); !strings.Contains(text, "-- [1] SKIPPED add column notes.body") {
		t.Errorf("EXPECTED the skipped delta in the text plan  ACTUAL:\n%s", text)
	}
}

// apply fails on a missing script, so the plan does too - naming the delta
func TestMissingScript(t *testing.T) {
	useManifest(t, writeManifest(t, map[string]string{
		"state/notes-table.json": notesTable,
		"deltas/v1_0_1.json":     `{"deltas":[{"object":"script","description":"backfill","script":"missing.sql"}]}`,
	}))
	_, err := ForChangesets(util.ExtractVersion("1.0.0"))
	if err == nil || !strings.Contains(err.Error(), "deltas/v1_0_1.json#/deltas/0") || !strings.Contains(err.Error(), "missing.sql") {
		t.Errorf("EXPECTED an error naming the delta and the script  ACTUAL: %v", err)
	}
}

// a table delta whose state file is missing panics in apply - the plan reports it as an error instead
func TestMissingTableDefinition(t *testing.T) {
	useManifest(t, writeManifest(t, map[string]string{
		"state/notes-table.json": notesTable,
		"deltas/v1_0_1.json":     `{"deltas":[{"object":"index","action":"add","name":"x_idx","reference":"absent"}]}`,
	}))
	_, err := ForChangesets(util.ExtractVersion("1.0.0"))
	if err == nil || !strings.Contains(err.Error(), "Unable to parse the reference table absent") {
		t.Errorf("EXPECTED an error about the missing table  ACTUAL: %v", err)
	}
}

func TestNoChangesetsStateVersion(t *testing.T) {
	useManifest(t, writeManifest(t, map[string]string{"state/notes-table.json": notesTable}))
	p, err := ForState(true)
	if err != nil {
		t.Fatal(err)
	}
	if p.To != "1.0.0" || p.From != "" {
		t.Errorf("EXPECTED a state build of 1.0.0 when there are no changesets  ACTUAL: from %q to %q", p.From, p.To)
	}
}

func TestHookedDeltas(t *testing.T) {
	hook := manifest.PrePostHook{Pre: manifest.Hook{Sql: []string{"select 1"}}, Post: manifest.Hook{Script: "post.sql"}}
	for name, delta := range map[string]manifest.Delta{
		"SimpleDelta": manifest.SimpleDelta{PrePostHook: hook},
		"ColumnDelta": manifest.ColumnDelta{PrePostHook: hook},
		"FkDelta":     manifest.FkDelta{PrePostHook: hook},
	} {
		d, ok := delta.(hooked)
		if !ok {
			t.Errorf("%s should support pre/post hooks", name)
			continue
		}
		if got := d.Hooks(); len(got.Pre.Sql) != 1 || got.Post.Script != "post.sql" {
			t.Errorf("%s returned the wrong hooks: %+v", name, got)
		}
	}
	if _, ok := manifest.Delta(manifest.SqlDelta{}).(hooked); ok {
		t.Error("SqlDelta should not support pre/post hooks")
	}
}

// a missing table (42P01) is tolerated only by drops - the object is gone either way. Anything else that needs the
// table fails when it is missing
func TestDeltaTolerates(t *testing.T) {
	for statementType, expected := range map[string]string{
		"delta add column":    "42P07,42710",
		"delta alter column":  "42P07,42710",
		"delta add view":      "42P07,42710",
		"delta add fk":        "42P07,42710",
		"delta rename column": "42P07,42710,42703",
		"delta drop table":    "42P07,42710,42704,42P01",
		"delta drop column":   "42P07,42710,42704,42P01",
		"delta drop fk":       "42P07,42710,42704,42P01",
	} {
		if actual := strings.Join(deltaTolerates(statementType), ","); actual != expected {
			t.Errorf("%s: EXPECTED %s  ACTUAL: %s", statementType, expected, actual)
		}
	}
}

// drift builds the state files without the post sql files, which load data
func TestWithoutPostSql(t *testing.T) {
	useManifest(t, fixture)
	p, err := ForState(true)
	if err != nil {
		t.Fatal(err)
	}
	all, schema := p.Units[0].Steps, p.Units[0].WithoutPostSql().Steps
	if len(schema) != len(all)-1 || schema[0].Source != "state/sql/pre/update_last_modified_at.sql" {
		t.Errorf("EXPECTED only state/sql/post/seed_projects.sql removed  ACTUAL: %d of %d steps kept", len(schema), len(all))
	}
	for _, step := range schema {
		if strings.HasPrefix(step.Source, "state/sql/post/") {
			t.Errorf("EXPECTED no post sql steps  ACTUAL: %s", step.Source)
		}
	}
}

// an added view or column is created as its state file describes, comment included - a column added from an
// explicit definition takes the definition's comment
func TestAddedComments(t *testing.T) {
	useManifest(t, writeManifest(t, map[string]string{
		"state/notes-table.json": `{"name":"notes","columns":[{"name":"id","datatype":"uuid"},{"name":"body","datatype":"text","comment":"The note"}]}`,
		"state/recent-view.json": `{"name":"recent","columns":["id"],"sql":"select id from notes","comment":"Latest notes"}`,
		"deltas/v1_0_1.json": `{"deltas":[
			{"object":"column","action":"add","name":"body","reference":"notes"},
			{"object":"column","action":"add","name":"draft","reference":"notes","def":{"name":"draft","datatype":"bool","comment":"Not published yet"}},
			{"object":"view","action":"add","name":"recent","post":{"sql":["select 1"]}}
		]}`,
	}))
	p, err := ForChangesets(util.ExtractVersion("1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, step := range p.Units[0].Steps {
		steps = append(steps, step.Kind+": "+step.Sql[0])
	}
	expected := []string{
		"delta: alter table notes add column if not exists body text not null",
		"comment: comment on column notes.body is 'The note'",
		"delta: alter table notes add column if not exists draft bool not null",
		"comment: comment on column notes.draft is 'Not published yet'",
		"delta: create or replace view recent (id) as select id from notes",
		// before the post hook - the delta creates the view with its comment, then the hook runs
		"comment: comment on view recent is 'Latest notes'",
		"post-hook: select 1",
	}
	if strings.Join(steps, "\n") != strings.Join(expected, "\n") {
		t.Errorf("EXPECTED\n  %s\nACTUAL:\n  %s", strings.Join(expected, "\n  "), strings.Join(steps, "\n  "))
	}
}
