package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"skima/pkg/plan"
	"skima/pkg/util"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spf13/viper"
)

// fakeExecer runs no sql - each statement fails with its queued errors in turn, then succeeds
type fakeExecer struct {
	errs     map[string][]error
	executed []string
}

func (f *fakeExecer) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	f.executed = append(f.executed, sql)
	if queued := f.errs[sql]; len(queued) > 0 {
		f.errs[sql] = queued[1:]
		return pgconn.CommandTag{}, queued[0]
	}
	return pgconn.CommandTag{}, nil
}

type row struct {
	stmtType, status, desc string
}

// fakeHistory collects the statement history rows an executor writes
type fakeHistory struct {
	rows []row
}

func (h *fakeHistory) record(history *ApplyHistory, stmtType string, status string, desc string) (int, error) {
	h.rows = append(h.rows, row{stmtType, status, desc})
	if status != "success" {
		history.Status = "failed" // as recordStatementHist does
	}
	return len(h.rows), nil
}

func (h *fakeHistory) count(status string) int {
	n := 0
	for _, r := range h.rows {
		if r.status == status {
			n++
		}
	}
	return n
}

func pgError(code string) error {
	return &pgconn.PgError{Severity: "ERROR", Code: code, Message: "error " + code}
}

// execute runs the steps against a fake database where each statement fails with its queued errors
func execute(steps []plan.Step, errs map[string][]error) (failures int, ex *fakeExecer, hist *fakeHistory, history *ApplyHistory) {
	ex = &fakeExecer{errs: errs}
	hist = &fakeHistory{}
	history = &ApplyHistory{Status: "started"}
	failures = executor{ex: ex, record: hist.record, history: history, out: io.Discard, errOut: io.Discard}.run(steps)
	return failures, ex, hist, history
}

func deltaStep(sql string, tolerate ...string) plan.Step {
	return plan.Step{Kind: plan.KindDelta, Describe: "add column t." + sql, Sql: []string{sql}, Tolerate: tolerate, StopOnFailure: true,
		Record: plan.Record{Type: "delta add column", Name: "add t." + sql}}
}

func viewStep(name string) plan.Step {
	return plan.Step{Kind: plan.KindView, Describe: name, Sql: []string{name}, Retryable: true, Record: plan.Record{Type: "Create View", Name: name}}
}

func TestChangesetStopsAtFirstFailure(t *testing.T) {
	failures, ex, hist, history := execute(
		[]plan.Step{deltaStep("one"), deltaStep("two"), deltaStep("three")},
		map[string][]error{"two": {pgError("42601")}})
	if failures != 1 || history.Status != "fast-fail" {
		t.Errorf("EXPECTED 1 failure and a fast-fail run  ACTUAL: %d failures, status %s", failures, history.Status)
	}
	if strings.Join(ex.executed, ",") != "one,two" {
		t.Errorf("EXPECTED the run to stop after the failed delta  ACTUAL: executed %v", ex.executed)
	}
	expected := []row{{"delta add column", "success", "add t.one"}, {"delta add column", "failed", "ERROR: error 42601 (SQLSTATE 42601)"}}
	if fmt.Sprint(hist.rows) != fmt.Sprint(expected) {
		t.Errorf("EXPECTED rows %v  ACTUAL: %v", expected, hist.rows)
	}
}

func TestToleratedErrors(t *testing.T) {
	// the codes the planner gives an add and a drop delta - only a drop tolerates a missing table (42P01)
	add := []string{"42P07", "42710"}
	drop := []string{"42P07", "42710", "42704", "42P01"}
	failures, _, hist, _ := execute(
		[]plan.Step{deltaStep("exists", add...), deltaStep("dropped_table", drop...)},
		map[string][]error{"exists": {pgError("42710")}, "dropped_table": {pgError("42P01")}})
	if failures != 0 || hist.count("success") != 2 {
		t.Errorf("EXPECTED both tolerated errors to succeed  ACTUAL: %d failures, rows %v", failures, hist.rows)
	}
	if hist.rows[0].desc != "add t.exists" {
		t.Errorf("EXPECTED a tolerated step described by its name  ACTUAL: %v", hist.rows[0])
	}

	failures, _, hist, history := execute([]plan.Step{deltaStep("missing_table", add...)}, map[string][]error{"missing_table": {pgError("42P01")}})
	if failures != 1 || history.Status != "fast-fail" || hist.rows[0].status != "failed" {
		t.Errorf("EXPECTED an add to a missing table to fail the changeset  ACTUAL: %d failures, status %s, rows %v", failures, history.Status, hist.rows)
	}
}

func TestPerStatementStep(t *testing.T) {
	step := plan.Step{Kind: plan.KindScript, Describe: "seed", Sql: []string{"insert a", "insert b"}, Tolerate: []string{"23505"}, StopOnFailure: true,
		Record: plan.Record{Type: "delta", PerStatement: true}}
	failures, _, hist, _ := execute([]plan.Step{step}, map[string][]error{"insert b": {pgError("23505")}})
	expected := []row{{"delta sql [0]", "success", "insert a"}, {"delta sql [1]", "success", "{[23505] error 23505} - insert b"}}
	if failures != 0 || fmt.Sprint(hist.rows) != fmt.Sprint(expected) {
		t.Errorf("EXPECTED rows %v  ACTUAL: %d failures, rows %v", expected, failures, hist.rows)
	}

	// a failed sql delta leaves the run 'failed', where other steps leave it 'fast-fail'
	if _, _, _, history := execute([]plan.Step{step}, map[string][]error{"insert a": {pgError("42601")}}); history.Status != "failed" {
		t.Errorf("EXPECTED a failed sql delta to mark the run failed  ACTUAL: %s", history.Status)
	}

	hook := plan.Step{Kind: plan.KindPreHook, Describe: "alter column t.c", Sql: []string{"update a", "update b"}, StopOnFailure: true,
		Record: plan.Record{Type: "pre-hook", PerStatement: true}}
	failures, ex, hist, history := execute([]plan.Step{hook, deltaStep("alter")}, map[string][]error{"update a": {pgError("23502")}})
	expected = []row{{"pre-hook sql [0]", "failed", "{[23502] error 23502} - update a"}}
	if failures != 1 || len(ex.executed) != 1 || history.Status != "fast-fail" || fmt.Sprint(hist.rows) != fmt.Sprint(expected) {
		t.Errorf("EXPECTED the failed pre hook to stop the changeset with rows %v  ACTUAL: %d failures, executed %v, rows %v", expected, failures, ex.executed, hist.rows)
	}
}

func TestScriptHookFailureLabel(t *testing.T) {
	step := plan.Step{Kind: plan.KindPostHook, Describe: "add column t.c", Script: "deltas/scripts/backfill.sql", Sql: []string{"backfill"}, StopOnFailure: true,
		Record: plan.Record{Type: "Delta script hook", FailType: "Delta script hook [backfill.sql]", Name: "backfill.sql"}}
	if _, _, hist, _ := execute([]plan.Step{step}, nil); fmt.Sprint(hist.rows) != fmt.Sprint([]row{{"Delta script hook", "success", "backfill.sql"}}) {
		t.Errorf("EXPECTED a success row for the script hook  ACTUAL: %v", hist.rows)
	}
	_, _, hist, _ := execute([]plan.Step{step}, map[string][]error{"backfill": {pgError("42601")}})
	if len(hist.rows) != 1 || hist.rows[0].stmtType != "Delta script hook [backfill.sql]" || hist.rows[0].status != "failed" {
		t.Errorf("EXPECTED a failed row typed with the script name  ACTUAL: %v", hist.rows)
	}
}

func TestSkippedStep(t *testing.T) {
	skipped := plan.Step{Kind: plan.KindDelta, Describe: "add column t.gone", Skipped: true, StopOnFailure: true, Record: plan.Record{Type: "delta add column"}}
	failures, ex, hist, _ := execute([]plan.Step{skipped, deltaStep("next")}, nil)
	if failures != 0 || strings.Join(ex.executed, ",") != "next" {
		t.Errorf("EXPECTED the skipped delta not to run or fail  ACTUAL: %d failures, executed %v", failures, ex.executed)
	}
	if hist.rows[0] != (row{"delta add column", "deltaSkipped", "No DDL Generated"}) {
		t.Errorf("EXPECTED a deltaSkipped row  ACTUAL: %v", hist.rows[0])
	}
}

func TestStateBuildContinuesAfterFailure(t *testing.T) {
	table := func(name string) plan.Step {
		return plan.Step{Kind: plan.KindTable, Describe: name, Sql: []string{name}, Record: plan.Record{Type: "Create Table", Name: name}}
	}
	failures, ex, hist, _ := execute([]plan.Step{table("a"), table("b"), table("c")}, map[string][]error{"a": {pgError("42601")}, "c": {pgError("42601")}})
	if failures != 2 || len(ex.executed) != 3 || hist.count("success") != 1 {
		t.Errorf("EXPECTED every table to be tried, with 2 failures  ACTUAL: %d failures, executed %v, rows %v", failures, ex.executed, hist.rows)
	}

	pre := plan.Step{Kind: plan.KindSql, Describe: "fn.sql", Script: "state/sql/pre/fn.sql", Sql: []string{"fn"}, StopOnFailure: true, Record: plan.Record{Type: "sql", Name: "fn.sql"}}
	failures, ex, _, _ = execute([]plan.Step{pre, table("a")}, map[string][]error{"fn": {pgError("42601")}})
	if failures != 1 || len(ex.executed) != 1 {
		t.Errorf("EXPECTED a failed pre sql file to stop the build  ACTUAL: %d failures, executed %v", failures, ex.executed)
	}
}

func TestViewRetries(t *testing.T) {
	post := plan.Step{Kind: plan.KindSql, Describe: "seed.sql", Script: "state/sql/post/seed.sql", Sql: []string{"seed"}, Record: plan.Record{Type: "sql", Name: "seed.sql"}}
	// "first" selects from "second", so it fails until "second" exists - the retry happens before the post sql
	failures, ex, hist, _ := execute([]plan.Step{viewStep("first"), viewStep("second"), post}, map[string][]error{"first": {pgError("42P01")}})
	if failures != 0 || strings.Join(ex.executed, ",") != "first,second,first,seed" {
		t.Errorf("EXPECTED the view to be retried before the post sql  ACTUAL: %d failures, executed %v", failures, ex.executed)
	}
	if hist.count("failed") != 1 || hist.count("success") != 3 {
		t.Errorf("EXPECTED the failed first attempt and 3 successes to be recorded  ACTUAL: %v", hist.rows)
	}

	// a view's comment is retried with its view
	comment := plan.Step{Kind: plan.KindComment, Describe: "view first", Sql: []string{"comment first"}, Retryable: true,
		Record: plan.Record{Type: "Comment", Name: "view first"}}
	failures, ex, _, _ = execute([]plan.Step{viewStep("first"), comment, viewStep("second"), post},
		map[string][]error{"first": {pgError("42P01")}, "comment first": {pgError("42P01")}})
	if failures != 0 || strings.Join(ex.executed, ",") != "first,comment first,second,first,comment first,seed" {
		t.Errorf("EXPECTED the comment retried after its view  ACTUAL: %d failures, executed %v", failures, ex.executed)
	}

	// a view that never resolves is retried until a pass makes no progress
	missing := []error{pgError("42P01"), pgError("42P01"), pgError("42P01")}
	failures, ex, _, _ = execute([]plan.Step{viewStep("orphan")}, map[string][]error{"orphan": missing})
	if failures != 1 || len(ex.executed) != 2 {
		t.Errorf("EXPECTED one retry of an unresolvable view and 1 failure  ACTUAL: %d failures, executed %v", failures, ex.executed)
	}

	// other failures are not retried
	failures, ex, _, _ = execute([]plan.Step{viewStep("broken")}, map[string][]error{"broken": {pgError("42601")}})
	if failures != 1 || len(ex.executed) != 1 {
		t.Errorf("EXPECTED a syntax error not to be retried  ACTUAL: %d failures, executed %v", failures, ex.executed)
	}
}

func TestConnectionError(t *testing.T) {
	failures, _, hist, _ := execute([]plan.Step{viewStep("v")}, map[string][]error{"v": {errors.New("conn closed")}})
	if failures != 1 || hist.rows[0] != (row{"Create View", "failed", "conn closed"}) {
		t.Errorf("EXPECTED a non-retryable failure recorded with the error  ACTUAL: %d failures, rows %v", failures, hist.rows)
	}
}

// a shadow build runs a step's Shadow sql in place of its Sql - apply always runs the Sql
func TestShadowStatements(t *testing.T) {
	step := plan.Step{Kind: plan.KindMatview, Describe: "doc_counts", Sql: []string{"create materialized view"},
		Shadow: []string{"create scratch view", "create stand-in table"}, Record: plan.Record{Type: "Create Materialized View", Name: "doc_counts"}}
	plain := deltaStep("other")
	for _, shadow := range []bool{false, true} {
		ex := &fakeExecer{errs: map[string][]error{}}
		hist := &fakeHistory{}
		e := executor{ex: ex, record: hist.record, history: &ApplyHistory{}, out: io.Discard, errOut: io.Discard, shadow: shadow}
		if failures := e.run([]plan.Step{step, plain}); failures != 0 {
			t.Fatalf("shadow %v: %d failures", shadow, failures)
		}
		expected := "create materialized view,other"
		if shadow {
			expected = "create scratch view,create stand-in table,other"
		}
		if strings.Join(ex.executed, ",") != expected {
			t.Errorf("shadow %v: EXPECTED %s  ACTUAL: %v", shadow, expected, ex.executed)
		}
	}
}

// executing a real plan writes the history rows apply always has - and one success row per planned statement
func TestPlanHistoryRows(t *testing.T) {
	viper.Set("workdir", "../plan/testdata/manifest")
	t.Cleanup(func() { viper.Set("workdir", "") })
	p, err := plan.ForChangesets(util.ExtractVersion("1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	successes := 0
	for _, unit := range p.Units {
		failures, _, hist, _ := execute(unit.Steps, nil)
		if failures != 0 {
			t.Fatalf("changeset %s failed: %v", unit.Version, hist.rows)
		}
		successes += hist.count("success")
		for _, r := range hist.rows {
			types = append(types, r.stmtType)
		}
	}
	expected := []string{
		"delta add enum", "Comment", "delta add sequence", "Comment", "Create Table", "delta add column", "Comment",
		"Delta script hook", "delta add index", "Alter Sequence", "Add FK", // 1.0.1
		"pre-hook sql [0]", "delta alter column", "delta add constraint", "delta rename column", "delta drop fk",
		"delta sql [0]", "delta sql [1]", "delta script sql [0]", "delta add comment", "delta drop comment", "delta alter enum",
		"delta alter sequence", // 1.0.2
		"delta sql [0]", "delta add function", "delta add view", "Comment",
		"delta add matview", "Create Index", "Comment", "delta add view", "delta refresh matview", // 1.0.3
	}
	if strings.Join(types, "|") != strings.Join(expected, "|") {
		t.Errorf("EXPECTED history rows\n  %v\nACTUAL:\n  %v", expected, types)
	}
	if successes != p.Statements() {
		t.Errorf("EXPECTED a success row per planned statement (%d)  ACTUAL: %d", p.Statements(), successes)
	}
}
