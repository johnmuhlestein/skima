package db

import (
	"context"
	"fmt"
	"os"
	"skima/pkg/plan"
	"skima/pkg/util"

	"github.com/jackc/pgx/v5/pgconn"
)

// ApplyUnit applies a planned run - a changeset, the state build or the state sql files of apply --sql - returning the
// number of failures. Unless the unit opts out it is applied in a single transaction, so any failure rolls the whole
// unit back and the schema is left as it was. The history is written on separate connections so it survives a rollback.
func ApplyUnit(history *ApplyHistory, unit plan.Unit) int {
	name, optOut := runName(unit)
	r := beginRun(history, name, unit.Transactional, optOut)
	if r == nil {
		return 1
	}
	return r.finish(executor{ex: r.ex, record: recordStatementHist, history: history}.run(unit.Steps))
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
type recordFunc func(history *ApplyHistory, stmtType string, status string, desc string) (int, error)

// executor runs a unit's planned steps in order, recording each in the history
type executor struct {
	ex      execer
	record  recordFunc
	history *ApplyHistory
}

// run executes the steps, returning the number of failures. A failed step with StopOnFailure stops the run, other
// failures are counted and the run carries on. Views that reference a relation that does not exist yet are retried
// once the views planned with them have been applied - they may depend on one of those
func (e executor) run(steps []plan.Step) int {
	failures := 0
	var retry []plan.Step
	for _, step := range steps {
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
	return failures + e.retryViews(retry)
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
	for pass := 0; pass < maxViewPasses && len(views) > 0; pass++ {
		fmt.Printf("Retrying %d views\n", len(views))
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
	return failures + len(views)
}

// skip records a delta that generates no DDL
func (e executor) skip(step plan.Step) {
	stmtId, err := e.record(e.history, step.Record.Type, "deltaSkipped", "No DDL Generated")
	fmt.Fprintf(os.Stderr, "WARNING: applying %s was skipped due to no DDL - this could be due to a downstream change that removed this reference object from the state definition - Statement ID: %d\n", step.Describe, stmtId)
	checkErr(err, "Unable to record the statement history for applying the delta")
}

// step executes the step's statements in order, stopping at the first that fails with an error the step does not
// tolerate. It returns whether the step succeeded and, when it did not, whether the failure was a missing relation
func (e executor) step(step plan.Step) (ok bool, missingRelation bool) {
	tolerated := util.FromSlice(step.Tolerate)
	var toleratedErr *pgconn.PgError
	for idx, sql := range step.Sql {
		_, err := e.ex.Exec(context.Background(), sql)
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
		stmtId, err := e.record(e.history, step.Record.Type, "success", step.Record.Name)
		if toleratedErr != nil {
			fmt.Printf("SUCCESS: %s - tolerated [%s] %s - Statement ID: %d\n", stepName(step), toleratedErr.Code, toleratedErr.Message, stmtId)
		} else {
			fmt.Printf("SUCCESS: %s - Statement ID: %d\n", stepName(step), stmtId)
		}
		checkErr(err, "Unable to record the statement history for "+stepName(step))
	}
	return true, false
}

// statementSucceeded records one statement of a per-statement step - one that failed with a tolerated error is recorded
// as a success, described by the error
func (e executor) statementSucceeded(step plan.Step, idx int, sql string, pgErr *pgconn.PgError) {
	stmtType := fmt.Sprintf("%s sql [%d]", step.Record.Type, idx)
	if pgErr != nil {
		stmtId, err := e.record(e.history, stmtType, "success", fmt.Sprintf("{[%s] %s} - %s", pgErr.Code, pgErr.Message, sql))
		fmt.Printf("SUCCESS: sql %s at index [%d] marked as success because failure was due to acceptable condition [%s] %s  Statement ID: %d\n", step.Record.Type, idx, pgErr.Code, pgErr.Message, stmtId)
		checkErr(err, "Unable to record the statement history for 'SQL statement'")
		return
	}
	stmtId, err := e.record(e.history, stmtType, "success", sql)
	fmt.Printf("SUCCESS: sql %s at index [%d] Statement ID: %d\n", step.Record.Type, idx, stmtId)
	checkErr(err, "Unable to record the statement history for 'SQL statement'")
}

// failed records the statement that failed - a per-statement step records the statement, other steps the step
func (e executor) failed(step plan.Step, idx int, sql string, execErr error, pgErr *pgconn.PgError) {
	if step.Record.PerStatement {
		stmtType := fmt.Sprintf("%s sql [%d]", step.Record.Type, idx)
		if pgErr != nil {
			stmtId, err := e.record(e.history, stmtType, "failed", fmt.Sprintf("{[%s] %s} - %s", pgErr.Code, pgErr.Message, sql))
			fmt.Fprintf(os.Stderr, "FAILURE: sql %s at index [%d] marked as failed because failure was due to unacceptable condition [%s] %s  Statement ID: %d\n", step.Record.Type, idx, pgErr.Code, pgErr.Message, stmtId)
			checkErr(err, "Unable to record the statement history for 'SQL statement'")
		} else {
			stmtId, err := e.record(e.history, stmtType, "failed", execErr.Error())
			fmt.Fprintf(os.Stderr, "FAILURE: executing sql based %s; statement index [%d] - Statement ID: %d\n%s\n", step.Record.Type, idx, stmtId, execErr)
			checkErr(err, "Unable to record the statement history for 'SQL statement'")
		}
		return
	}
	stmtType := step.Record.Type
	if step.Record.FailType != "" {
		stmtType = step.Record.FailType
	}
	stmtId, err := e.record(e.history, stmtType, "failed", execErr.Error())
	if pgErr != nil && pgErr.Code == codeUndefinedTable && step.Retryable {
		fmt.Fprintf(os.Stderr, "  %s references a relation that does not exist yet: %s\n", stepName(step), pgErr.Message)
	}
	if step.Script == "" {
		fmt.Println(sql) // a file's statements are not printed - it is named instead
	}
	fmt.Fprintf(os.Stderr, "FAILURE: %s - Statement ID: %d\n %s\n", stepName(step), stmtId, execErr)
	checkErr(err, "Unable to record the statement history for "+stepName(step))
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
