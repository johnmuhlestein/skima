package db

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/johnmuhlestein/skima/internal/plan"
	"github.com/johnmuhlestein/skima/internal/util"
)

// ApplyUnit applies a planned run - a changeset, the state build or the state sql files of apply --sql - returning the
// number of failures. Unless the unit opts out it is applied in a single transaction, so any failure rolls the whole
// unit back and the schema is left as it was. The history is written on separate connections so it survives a rollback.
//
// Cancelling ctx (Ctrl+C) stops the run at the statement it is running: the transaction is rolled back and the run is
// marked 'cancelled'. Progress goes to out, failures to errOut
func (d *DB) ApplyUnit(ctx context.Context, history *ApplyHistory, unit plan.Unit, out, errOut io.Writer) (int, error) {
	name, optOut := runName(unit)
	r, err := d.beginRun(ctx, history, name, unit.Transactional, optOut, out, errOut)
	if err != nil {
		history.Status = "failed"
		return 1, err
	}
	failures := executor{ctx: ctx, ex: r.ex, record: d.recordStatementHist, history: history, out: out, errOut: errOut}.run(unit.Steps)
	return r.finish(ctx, failures), nil
}

// runName names the run in messages, along with how to apply it without a transaction
func runName(unit plan.Unit) (name string, optOut string) {
	stateOptOut := "set state.transaction to false (--state-transaction=false or SKM_STATE_TRANSACTION=false)"
	switch unit.Kind {
	case plan.UnitState:
		return "state build", stateOptOut
	case plan.UnitStateSql:
		return "state sql run", stateOptOut
	default:
		return "changeset " + unit.Version, `set "transaction": false on the changeset`
	}
}

// maxViewPasses bounds how many times the views that reference a relation created after them are retried
const maxViewPasses = 5

const codeUndefinedTable = "42P01" // relation does not exist

// recordFunc writes a row to the statements history table, returning its statement id
type recordFunc func(ctx context.Context, history *ApplyHistory, stmtType string, status string, desc string) (int, error)

// executor runs a unit's planned steps in order, recording each in the history
type executor struct {
	// ctx cancelled stops the run - see interrupted
	ctx     context.Context
	ex      execer
	record  recordFunc
	history *ApplyHistory
	// out and errOut receive the progress and failure messages
	out    io.Writer
	errOut io.Writer
	// shadow runs each step's Shadow sql in place of its Sql, when it has any - for a shadow build (skima drift)
	shadow bool
}

// run executes the steps, returning the number of failures. A failed step with StopOnFailure stops the run, other
// failures are counted and the run carries on. Views that reference a relation that does not exist yet are retried
// once the views planned with them have been applied - they may depend on one of those
func (e executor) run(steps []plan.Step) int {
	failures := 0
	var retry []plan.Step
	for _, step := range steps {
		if e.interrupted() {
			return failures + 1
		}
		if len(retry) > 0 && !step.Retryable {
			failures += e.retryViews(retry)
			retry = nil
		}
		if step.Skipped {
			e.skip(step)
			continue
		}
		ok, missingRelation := e.step(step)
		switch {
		case ok:
		case step.Retryable && missingRelation:
			retry = append(retry, step)
		default:
			failures++
			if step.StopOnFailure {
				e.history.Status = stoppedStatus(step)
				return failures
			}
		}
	}
	if e.interrupted() {
		return failures + 1
	}
	return failures + e.retryViews(retry)
}

// interrupted reports whether the run's context was cancelled (Ctrl+C) - the run stops and is marked 'cancelled'
func (e executor) interrupted() bool {
	if e.ctx.Err() == nil {
		return false
	}
	if e.history.Status != "cancelled" {
		e.history.Status = "cancelled"
		fmt.Fprintln(e.errOut, "Interrupted - the remaining steps were not run")
	}
	return true
}

// recordRow writes a statements history row - also after the context was cancelled, so the history says how far the
// run got. A row that can't be written is reported to errOut, as what, and the run carries on
func (e executor) recordRow(stmtType string, status string, desc string, what string) int {
	ctx, cancel := cleanup(e.ctx)
	defer cancel()
	stmtId, err := e.record(ctx, e.history, stmtType, status, desc)
	if err != nil {
		fmt.Fprintf(e.errOut, "Unable to record the statement history for %s: %s\n", what, err)
	}
	return stmtId
}

// stoppedStatus is the run status when the step's failure stops the run: a failed sql delta leaves the run 'failed',
// any other step (including a hook) 'fast-fail'. A transactional run is marked 'rolled back' once it is rolled back
func stoppedStatus(step plan.Step) string {
	if step.Kind == plan.KindScript {
		return "failed"
	}
	return "fast-fail"
}

// retryViews applies the views again until they are all created or a pass creates none of them, returning the number
// that could not be created
func (e executor) retryViews(views []plan.Step) int {
	failures := 0
	for pass := 0; pass < maxViewPasses && len(views) > 0 && !e.interrupted(); pass++ {
		fmt.Fprintf(e.out, "Retrying %d views\n", len(views))
		var again []plan.Step
		for _, view := range views {
			ok, missingRelation := e.step(view)
			if !ok && missingRelation {
				again = append(again, view)
			} else if !ok {
				failures++ // any other failure will not be fixed by trying again
			}
		}
		// no view was created this pass, so nothing changed that could make another pass succeed
		noProgress := len(again) == len(views)
		views = again
		if noProgress {
			break
		}
	}
	// the views still missing a relation are failures - their attempts so far are recorded as 'retrying'
	for _, view := range views {
		stmtType := view.Record.Type
		if view.Record.FailType != "" {
			stmtType = view.Record.FailType
		}
		stmtId := e.recordRow(stmtType, "failed", "still references a relation that does not exist after retrying", stepName(view))
		fmt.Fprintf(e.errOut, "FAILURE: %s still references a relation that does not exist - Statement ID: %d\n", stepName(view), stmtId)
	}
	return failures + len(views)
}

// skip records a delta that generates no DDL
func (e executor) skip(step plan.Step) {
	stmtId := e.recordRow(step.Record.Type, "deltaSkipped", "No DDL Generated", "skipping "+stepName(step))
	fmt.Fprintf(e.errOut, "WARNING: applying %s was skipped due to no DDL - this could be due to a downstream change that removed this reference object from the state definition - Statement ID: %d\n", step.Describe, stmtId)
}

// step executes the step's statements in order, stopping at the first that fails with an error the step does not
// tolerate. It returns whether the step succeeded and, when it did not, whether the failure was a missing relation
func (e executor) step(step plan.Step) (ok bool, missingRelation bool) {
	tolerated := util.FromSlice(step.Tolerate)
	var toleratedErr *pgconn.PgError
	statements := step.Sql
	if e.shadow && len(step.Shadow) > 0 {
		statements = step.Shadow
	}
	for idx, sql := range statements {
		_, err := e.ex.Exec(e.ctx, sql)
		pgErr := asPgError(err)
		if err != nil && (pgErr == nil || !tolerated.Has(pgErr.Code)) {
			e.failed(step, idx, sql, err, pgErr)
			return false, pgErr != nil && pgErr.Code == codeUndefinedTable
		}
		if step.Record.PerStatement {
			e.statementSucceeded(step, idx, sql, pgErr)
		} else if pgErr != nil {
			toleratedErr = pgErr
		}
	}
	if !step.Record.PerStatement {
		stmtId := e.recordRow(step.Record.Type, "success", step.Record.Name, stepName(step))
		if toleratedErr != nil {
			fmt.Fprintf(e.out, "SUCCESS: %s - tolerated [%s] %s - Statement ID: %d\n", stepName(step), toleratedErr.Code, toleratedErr.Message, stmtId)
		} else {
			fmt.Fprintf(e.out, "SUCCESS: %s - Statement ID: %d\n", stepName(step), stmtId)
		}
	}
	return true, false
}

// statementSucceeded records one statement of a per-statement step - one that failed with a tolerated error is recorded
// as a success, described by the error
func (e executor) statementSucceeded(step plan.Step, idx int, sql string, pgErr *pgconn.PgError) {
	stmtType := fmt.Sprintf("%s sql [%d]", step.Record.Type, idx)
	if pgErr != nil {
		stmtId := e.recordRow(stmtType, "success", fmt.Sprintf("{[%s] %s} - %s", pgErr.Code, pgErr.Message, sql), "the sql statement")
		fmt.Fprintf(e.out, "SUCCESS: sql %s at index [%d] marked as success because failure was due to acceptable condition [%s] %s  Statement ID: %d\n", step.Record.Type, idx, pgErr.Code, pgErr.Message, stmtId)
		return
	}
	stmtId := e.recordRow(stmtType, "success", sql, "the sql statement")
	fmt.Fprintf(e.out, "SUCCESS: sql %s at index [%d] Statement ID: %d\n", step.Record.Type, idx, stmtId)
}

// failed records the statement that failed - a per-statement step records the statement, other steps the step
func (e executor) failed(step plan.Step, idx int, sql string, execErr error, pgErr *pgconn.PgError) {
	if step.Record.PerStatement {
		stmtType := fmt.Sprintf("%s sql [%d]", step.Record.Type, idx)
		if pgErr != nil {
			stmtId := e.recordRow(stmtType, "failed", fmt.Sprintf("{[%s] %s} - %s", pgErr.Code, pgErr.Message, sql), "the sql statement")
			fmt.Fprintf(e.errOut, "FAILURE: sql %s at index [%d] marked as failed because failure was due to unacceptable condition [%s] %s  Statement ID: %d\n", step.Record.Type, idx, pgErr.Code, pgErr.Message, stmtId)
		} else {
			stmtId := e.recordRow(stmtType, "failed", execErr.Error(), "the sql statement")
			fmt.Fprintf(e.errOut, "FAILURE: executing sql based %s; statement index [%d] - Statement ID: %d\n%s\n", step.Record.Type, idx, stmtId, execErr)
		}
		return
	}
	stmtType := step.Record.Type
	if step.Record.FailType != "" {
		stmtType = step.Record.FailType
	}
	if pgErr != nil && pgErr.Code == codeUndefinedTable && step.Retryable {
		// retried once the steps planned with it have run - recorded as 'retrying', so a view that succeeds on retry
		// leaves no failed row in a successful run
		stmtId := e.recordRow(stmtType, "retrying", execErr.Error(), stepName(step))
		fmt.Fprintf(e.out, "RETRYING: %s references a relation that does not exist yet: %s - Statement ID: %d\n", stepName(step), pgErr.Message, stmtId)
		return
	}
	stmtId := e.recordRow(stmtType, "failed", execErr.Error(), stepName(step))
	if step.Script == "" {
		fmt.Fprintln(e.out, sql) // a file's statements are not printed - it is named instead
	}
	fmt.Fprintf(e.errOut, "FAILURE: %s - Statement ID: %d\n %s\n", stepName(step), stmtId, execErr)
}

// stepName describes the step in messages, e.g. "table documents" or "add column documents.priority"
func stepName(step plan.Step) string {
	name := step.Describe
	if step.Kind != plan.KindDelta {
		name = step.Kind + " " + name
	}
	if step.Script != "" {
		name += " (" + step.Script + ")"
	}
	return name
}
