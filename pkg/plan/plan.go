// Package plan works out what skima apply would run against a schema - every statement, in the order it runs them -
// without connecting to a database. The manifest is read from the configured working directory, like the rest of skima.
package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"skima/pkg/manifest"
	"skima/pkg/util"
	"strings"

	"github.com/spf13/viper"
)

// Step kinds
const (
	KindSql        = "sql" // a state sql file
	KindTable      = "table"
	KindIndex      = "index"
	KindTrigger    = "trigger"
	KindConstraint = "constraint"
	KindFk         = "fk"
	KindView       = "view"
	KindDelta      = "delta"
	KindScript     = "script" // a script delta - sql statements or a file in deltas/scripts
	KindPreHook    = "pre-hook"
	KindPostHook   = "post-hook"
)

// postgres error codes that apply treats as success - see pkg/db
const (
	codeDuplicateObject = "42710" // e.g. a trigger, check constraint or fk that already exists
	codeDuplicateTable  = "42P07" // e.g. a unique constraint's index that already exists
	codeUndefinedTable  = "42P01"
	codeUndefinedColumn = "42703"
	codeUndefinedObject = "42704"
)

// Step is one thing apply runs: a statement, a sql file, or the statements of a sql delta or hook
type Step struct {
	Kind     string `json:"kind"`
	Describe string `json:"describe"`
	// Source is the manifest file the step comes from, relative to the working directory - for a changeset with a JSON
	// pointer to the delta, e.g. deltas/v1_0_2.json#/deltas/3
	Source string `json:"source"`
	// Delta is the 1-based position of the delta in its changeset, 0 for state build steps
	Delta int `json:"delta,omitempty"`
	// Script is the sql file the step runs, relative to the working directory - its contents are the step's one statement
	Script string   `json:"script,omitempty"`
	Sql    []string `json:"sql,omitempty"`
	// Tolerate lists the postgres error codes apply treats as success for this step
	Tolerate []string `json:"tolerate,omitempty"`
	// Retryable is set on state build views - apply retries a view that references a relation created after it
	Retryable bool `json:"retryable,omitempty"`
	// Deferred is set on a changeset's foreign keys of added tables - they are added after every delta
	Deferred bool `json:"deferred,omitempty"`
	// Skipped is set when the delta generates no statement - apply records it as skipped and does not run its post hook
	Skipped bool   `json:"skipped,omitempty"`
	Note    string `json:"note,omitempty"`
	// StopOnFailure stops the run when the step fails - every changeset step, and the state build's pre sql files that
	// the rest of the build depends on. Other state build steps fail without stopping, so one run reports every failure
	StopOnFailure bool `json:"-"`
	// Record is how apply records the step in the skima_schema_statements history table
	Record Record `json:"-"`
}

// Record is how apply records a step in the skima_schema_statements history table
type Record struct {
	// Type is the statement type, e.g. "Create Table" or "delta add column"
	Type string
	// FailType is the statement type when the step fails, if it differs from Type
	FailType string
	// Name is the description of the step when it succeeds
	Name string
	// PerStatement records each statement on its own, as "<Type> sql [<index>]" described by the statement - used for
	// sql deltas and hooks
	PerStatement bool
}

// Unit kinds
const (
	UnitState     = "state"
	UnitStateSql  = "sql" // the state build's pre sql files, applied on their own by apply --sql
	UnitChangeset = "changeset"
)

// Unit is one apply run: the state build or a changeset. Each is recorded as a run in the history tables and, unless
// it opts out, applied in a single transaction
type Unit struct {
	Kind          string               `json:"kind"` // state, sql or changeset
	SemVer        util.SemanticVersion `json:"-"`
	Version       string               `json:"version"`
	File          string               `json:"file,omitempty"`
	Description   string               `json:"description,omitempty"`
	Transactional bool                 `json:"transactional"`
	Steps         []Step               `json:"steps"`
}

// Plan is everything an apply would run, in order
type Plan struct {
	Schema string `json:"schema,omitempty"`
	// From is the deployed version - empty when skima does not manage the schema yet, so apply builds it from state
	From     string   `json:"from"`
	To       string   `json:"to"`
	Units    []Unit   `json:"units"`
	Warnings []string `json:"warnings,omitempty"`
}

// Pending reports whether applying would change anything
func (p Plan) Pending() bool {
	return len(p.Units) > 0
}

// Statements counts the statements the plan runs - a sql file is one statement
func (p Plan) Statements() int {
	count := 0
	for _, unit := range p.Units {
		for _, step := range unit.Steps {
			if !step.Skipped {
				count += len(step.Sql)
			}
		}
	}
	return count
}

// ForState plans the state build that apply runs against a schema skima does not manage yet: the pre sql files, tables,
// foreign keys, views and post sql files. transactional is the state.transaction setting
func ForState(transactional bool) (Plan, error) {
	version := manifest.Version()
	unit := Unit{Kind: UnitState, SemVer: version, Version: version.String(), Transactional: transactional}
	workdir := viper.GetString("workdir")

	// the rest of the build depends on the pre sql files, e.g. trigger functions
	pre, err := sqlFileSteps("pre", true)
	if err != nil {
		return Plan{}, err
	}
	unit.Steps = append(unit.Steps, pre...)

	tablePaths, err := filepath.Glob(filepath.Join(workdir, "state", "*-table.json"))
	if err != nil {
		return Plan{}, err
	}
	var fks []Step
	for _, path := range tablePaths {
		table, err := manifest.ParseTableByPath(path)
		if err != nil {
			return Plan{}, fmt.Errorf("unable to parse %s: %w", relative(path), err)
		}
		unit.Steps = append(unit.Steps, tableSteps(table, relative(path), 0)...)
		fks = append(fks, fkSteps(table, relative(path), 0)...)
	}
	// foreign keys are added once every table exists
	unit.Steps = append(unit.Steps, fks...)

	viewPaths, err := filepath.Glob(filepath.Join(workdir, "state", "*-view.json"))
	if err != nil {
		return Plan{}, err
	}
	for _, path := range viewPaths {
		view, err := manifest.ParseViewByPath(path)
		if err != nil {
			return Plan{}, fmt.Errorf("unable to parse %s: %w", relative(path), err)
		}
		unit.Steps = append(unit.Steps, Step{Kind: KindView, Describe: view.Name, Source: relative(path), Sql: []string{view.GenerateDdl()}, Retryable: true,
			Record: Record{Type: "Create View", Name: view.Name}})
	}

	post, err := sqlFileSteps("post", false)
	if err != nil {
		return Plan{}, err
	}
	unit.Steps = append(unit.Steps, post...)

	p := Plan{Schema: viper.GetString("dbconn.schema"), To: unit.Version, Units: []Unit{unit}}
	if !transactional {
		p.Warnings = append(p.Warnings, "the state build runs without a transaction (state.transaction is false) - a failure part way through leaves the earlier objects in place")
	}
	return p, nil
}

// ForStateSql plans apply --sql: only the state build's pre sql files, each applied even when an earlier one fails
func ForStateSql(transactional bool) (Plan, error) {
	steps, err := sqlFileSteps("pre", false)
	if err != nil {
		return Plan{}, err
	}
	version := manifest.Version()
	unit := Unit{Kind: UnitStateSql, SemVer: version, Version: version.String(), Transactional: transactional, Steps: steps}
	return Plan{Schema: viper.GetString("dbconn.schema"), To: unit.Version, Units: []Unit{unit}}, nil
}

// ForChangesets plans the changesets newer than the deployed version, in the order apply runs them
func ForChangesets(deployed util.SemanticVersion) (Plan, error) {
	p := Plan{Schema: viper.GetString("dbconn.schema"), From: deployed.String(), To: deployed.String()}
	files, err := manifest.ListFiles(filepath.Join(viper.GetString("workdir"), "deltas"), "json")
	if err != nil {
		return Plan{}, err
	}
	for _, path := range util.NewerVersions(deployed, files) {
		unit, warnings, err := changesetUnit(path)
		if err != nil {
			return Plan{}, err
		}
		p.Units = append(p.Units, unit)
		p.Warnings = append(p.Warnings, warnings...)
		p.To = unit.Version
	}
	return p, nil
}

func changesetUnit(path string) (Unit, []string, error) {
	file := relative(path)
	cs, err := manifest.ParseChangeSet(path)
	if err != nil {
		return Unit{}, nil, fmt.Errorf("unable to parse %s: %w", file, err)
	}
	unit := Unit{Kind: UnitChangeset, SemVer: cs.SemVer, Version: cs.Version, File: file, Description: cs.Description, Transactional: cs.InTransaction()}
	var warnings []string
	if !unit.Transactional {
		warnings = append(warnings, fmt.Sprintf("changeset %s runs without a transaction - a failure part way through leaves the earlier deltas in place", cs.Version))
	}
	var fks []Step
	for i, delta := range cs.ParsedDeltas {
		source := fmt.Sprintf("%s#/deltas/%d", file, i)
		position := i + 1
		fail := func(err error) (Unit, []string, error) {
			return Unit{}, nil, fmt.Errorf("%s: %w", source, err)
		}
		hooks, isHooked := delta.(hooked)
		if isHooked {
			step, err := hookStep(KindPreHook, hooks.Hooks().Pre, delta, source, position)
			if err != nil {
				return fail(err)
			}
			if step != nil {
				unit.Steps = append(unit.Steps, *step)
			}
		}

		switch d := delta.(type) {
		case manifest.SimpleDelta:
			if d.Object == "table" && d.Action == "add" {
				table, err := manifest.ParseTableByName(d.Name)
				if err != nil {
					return fail(fmt.Errorf("unable to parse the table %s: %w", d.Name, err))
				}
				unit.Steps = append(unit.Steps, tableSteps(table, source, position)...)
				// the foreign keys of added tables are added once every delta has run
				for _, fk := range fkSteps(table, source, position) {
					fk.Deferred = true
					fks = append(fks, fk)
				}
				break
			}
			ddl, err := d.Ddl()
			step, err := deltaStep(delta, ddl, err, source, position)
			if err != nil {
				return fail(err)
			}
			if d.Object == "function" && d.Action == "add" && !step.Skipped {
				if fnFile, err := d.FunctionFile(); err == nil {
					step.Script = relative(fnFile)
				}
			}
			unit.Steps = append(unit.Steps, step)
		case manifest.ColumnDelta:
			ddl, err := d.Ddl()
			if err != nil {
				// apply skips a column delta whose table cannot be read rather than failing
				unit.Steps = append(unit.Steps, Step{Kind: KindDelta, Describe: describe(d), Source: source, Delta: position, Skipped: true,
					Record: Record{Type: d.StatementType(), Name: d.Describe()},
					Note:   fmt.Sprintf("%s generates no DDL (unable to read the table %s: %s) - apply skips it", describe(d), d.Reference, err)})
				break
			}
			step, err := deltaStep(delta, ddl, nil, source, position)
			if err != nil {
				return fail(err)
			}
			unit.Steps = append(unit.Steps, step)
		case manifest.SqlDelta:
			statements, err := d.Statements()
			if err != nil {
				return fail(err)
			}
			step := Step{Kind: KindScript, Describe: d.Describe(), Source: source, Delta: position, Sql: statements, Tolerate: d.Bypass,
				Record: Record{Type: "delta", PerStatement: true}}
			if len(d.Script) > 0 {
				step.Script = relative(manifest.ScriptPath(viper.GetString("workdir"), d.Script))
				step.Record.Type = "delta script"
			}
			unit.Steps = append(unit.Steps, step)
		default:
			step, err := deltaStep(delta, delta.GenerateDdl(), nil, source, position)
			if err != nil {
				return fail(err)
			}
			unit.Steps = append(unit.Steps, step)
		}

		last := unit.Steps[len(unit.Steps)-1]
		if last.Skipped {
			warnings = append(warnings, fmt.Sprintf("%s: %s", source, last.Note))
			continue // apply does not run the post hook of a skipped delta
		}
		if isHooked {
			step, err := hookStep(KindPostHook, hooks.Hooks().Post, delta, source, position)
			if err != nil {
				return fail(err)
			}
			if step != nil {
				unit.Steps = append(unit.Steps, *step)
			}
		}
	}
	unit.Steps = append(unit.Steps, fks...)
	// a changeset stops at its first failure
	for i := range unit.Steps {
		unit.Steps[i].StopOnFailure = true
	}
	return unit, warnings, nil
}

// deltaStep is the step for a delta that generates a single statement. A delta whose definition is missing from the
// state files generates none - apply skips it
func deltaStep(delta manifest.Delta, ddl string, err error, source string, position int) (Step, error) {
	step := Step{Kind: KindDelta, Describe: describe(delta), Source: source, Delta: position,
		Record: Record{Type: delta.StatementType(), Name: delta.Describe()}}
	var missing *manifest.MissingDefinitionError
	switch {
	case errors.As(err, &missing):
		step.Skipped = true
		step.Note = fmt.Sprintf("%s generates no DDL (%s) - apply skips it", describe(delta), missing)
	case err != nil:
		return step, err
	case len(ddl) == 0:
		step.Skipped = true
		step.Note = fmt.Sprintf("%s generates no DDL - apply skips it", describe(delta))
	default:
		step.Sql = []string{ddl}
		step.Tolerate = deltaTolerates(delta.StatementType())
	}
	return step, nil
}

// describe names the delta's object along with what it does, e.g. "add column documents.priority" - a delta's
// Describe leaves the object out
func describe(delta manifest.Delta) string {
	statementType := strings.Fields(delta.StatementType()) // "delta <action> <object>"
	if len(statementType) != 3 || statementType[0] != "delta" {
		return delta.Describe()
	}
	action, object := statementType[1], statementType[2]
	return fmt.Sprintf("%s %s %s", action, object, strings.TrimPrefix(delta.Describe(), action+" "))
}

// deltaTolerates returns the error codes apply treats as success for a delta's statement
func deltaTolerates(statementType string) []string {
	codes := []string{codeDuplicateTable, codeDuplicateObject, codeUndefinedTable}
	if strings.Contains(statementType, "rename") {
		codes = append(codes, codeUndefinedColumn)
	}
	if strings.Contains(statementType, "drop") {
		codes = append(codes, codeUndefinedObject)
	}
	return codes
}

// hooked is implemented by the deltas that support pre/post hooks - those that embed manifest.PrePostHook
type hooked interface {
	Hooks() manifest.PrePostHook
}

func hookStep(kind string, hook manifest.Hook, delta manifest.Delta, source string, position int) (*Step, error) {
	statements, err := hook.Statements()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	if len(statements) == 0 {
		return nil, nil
	}
	step := Step{Kind: kind, Describe: describe(delta), Source: source, Delta: position, Sql: statements,
		Record: Record{Type: kind, PerStatement: true}}
	if len(hook.Sql) == 0 {
		step.Script = relative(manifest.ScriptPath(viper.GetString("workdir"), hook.Script))
		step.Record = Record{Type: "Delta script hook", FailType: fmt.Sprintf("Delta script hook [%s]", hook.Script), Name: hook.Script}
	}
	return &step, nil
}

// tableSteps creates the table with its indexes, triggers and constraints - its foreign keys are added separately
func tableSteps(table manifest.Table, source string, position int) []Step {
	steps := []Step{{Kind: KindTable, Describe: table.Name, Source: source, Delta: position, Sql: []string{table.GenerateDdl()},
		Record: Record{Type: "Create Table", Name: table.Name}}}
	for _, idx := range table.Indexes {
		name := table.Name + "." + idx.Name
		steps = append(steps, Step{Kind: KindIndex, Describe: name, Source: source, Delta: position, Sql: []string{idx.GenerateDdl(table.Name)},
			Record: Record{Type: "Create Index", Name: name}})
	}
	for _, trg := range table.Triggers {
		name := table.Name + "." + trg.Name
		steps = append(steps, Step{Kind: KindTrigger, Describe: name, Source: source, Delta: position, Sql: []string{trg.Ddl},
			Tolerate: []string{codeDuplicateObject}, Record: Record{Type: "Create Trigger", Name: name}})
	}
	for _, cons := range table.Constraints {
		name := table.Name + "." + cons.Name
		steps = append(steps, Step{Kind: KindConstraint, Describe: name, Source: source, Delta: position, Sql: []string{cons.GenerateDdl(table.Name)},
			Tolerate: []string{codeDuplicateTable, codeDuplicateObject}, Record: Record{Type: "Add Constraint", Name: name}})
	}
	return steps
}

func fkSteps(table manifest.Table, source string, position int) []Step {
	var steps []Step
	for _, fk := range table.ForeignKeys {
		name := table.Name + "." + fk.Name
		steps = append(steps, Step{Kind: KindFk, Describe: name, Source: source, Delta: position, Sql: []string{fk.AlterStmt(table.Name, "add")},
			Tolerate: []string{codeDuplicateObject}, Record: Record{Type: "Add FK", Name: name}})
	}
	return steps
}

// sqlFileSteps runs the state sql files of the phase (pre or post), in file name order
func sqlFileSteps(phase string, stopOnFailure bool) ([]Step, error) {
	paths, err := manifest.ListFiles(filepath.Join(viper.GetString("workdir"), "state", "sql", phase), "sql")
	if err != nil {
		return nil, err
	}
	var steps []Step
	for _, path := range paths {
		dat, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("unable to read %s: %w", relative(path), err)
		}
		steps = append(steps, Step{Kind: KindSql, Describe: filepath.Base(path), Source: relative(path), Script: relative(path), Sql: []string{string(dat)},
			StopOnFailure: stopOnFailure, Record: Record{Type: "sql", Name: filepath.Base(path)}})
	}
	return steps, nil
}

// relative shows a manifest path relative to the working directory, with forward slashes
func relative(path string) string {
	if rel, err := filepath.Rel(viper.GetString("workdir"), path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}
