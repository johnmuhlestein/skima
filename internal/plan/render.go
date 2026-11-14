package plan

import (
	"fmt"
	"io"
	"strings"
)

// RenderOptions controls the text rendering of a plan
type RenderOptions struct {
	// ShowScripts prints the contents of sql files - by default a step that runs a file names it instead
	ShowScripts bool
}

// Render writes the plan as text: a summary, then each run's statements under a comment describing the step, so the
// output reads close to the sql that would run
func Render(w io.Writer, p Plan, opts RenderOptions) {
	schema := ""
	if p.Schema != "" {
		schema = " for schema " + p.Schema
	}
	if !p.Pending() {
		fmt.Fprintf(w, "No changes%s: version %s is deployed and there are no newer changesets\n", schema, p.From)
		renderWarnings(w, p)
		return
	}
	if p.From == "" {
		fmt.Fprintf(w, "Plan%s: not managed by skima yet - build version %s from the state files (%s)\n", schema, p.To, plural(p.Statements(), "statement"))
	} else {
		fmt.Fprintf(w, "Plan%s: deployed %s -> %s (%s, %s)\n", schema, p.From, p.To, plural(len(p.Units), "changeset"), plural(p.Statements(), "statement"))
	}

	for _, unit := range p.Units {
		fmt.Fprintln(w)
		transaction := "[transaction]"
		if !unit.Transactional {
			transaction = "[NO TRANSACTION - a failure part way through is not rolled back]"
		}
		switch unit.Kind {
		case UnitState:
			fmt.Fprintf(w, "== state build %s  %s\n", unit.Version, transaction)
		case UnitStateSql:
			fmt.Fprintf(w, "== state pre sql files %s  %s\n", unit.Version, transaction)
		default:
			fmt.Fprintf(w, "== changeset %s  %s  %s\n", unit.Version, unit.File, transaction)
			if unit.Description != "" {
				fmt.Fprintf(w, "-- %s\n", unit.Description)
			}
		}
		for _, step := range unit.Steps {
			renderStep(w, step, opts)
		}
	}

	fmt.Fprintln(w)
	renderWarnings(w, p)
}

func renderStep(w io.Writer, step Step, opts RenderOptions) {
	var bld strings.Builder
	bld.WriteString("-- ")
	switch {
	case step.Deferred:
		bld.WriteString("[end] ")
	case step.Delta > 0:
		fmt.Fprintf(&bld, "[%d] ", step.Delta)
	}
	if step.Skipped {
		bld.WriteString("SKIPPED ")
	}
	if step.Kind != KindDelta {
		bld.WriteString(step.Kind)
		bld.WriteString(" ")
	}
	bld.WriteString(step.Describe)
	var notes []string
	if step.Deferred {
		notes = append(notes, "deferred to the end of the changeset")
	}
	if step.Retryable {
		notes = append(notes, "retried if it depends on a view or materialized view created after it")
	}
	if len(step.Tolerate) > 0 {
		notes = append(notes, "tolerates "+strings.Join(step.Tolerate, ", "))
	}
	if len(notes) > 0 {
		fmt.Fprintf(&bld, "  (%s)", strings.Join(notes, "; "))
	}
	fmt.Fprintln(w, bld.String())

	switch {
	case step.Skipped:
		fmt.Fprintf(w, "--   %s\n", step.Note)
	case step.Script != "" && !opts.ShowScripts:
		fmt.Fprintf(w, "--   runs %s (--show-scripts prints it)\n", step.Script)
	default:
		if step.Script != "" {
			fmt.Fprintf(w, "--   %s:\n", step.Script)
		}
		for _, sql := range step.Sql {
			fmt.Fprintln(w, terminate(sql))
		}
	}
}

func renderWarnings(w io.Writer, p Plan) {
	for _, warning := range p.Warnings {
		fmt.Fprintf(w, "WARNING: %s\n", warning)
	}
}

// terminate ends a statement with a semicolon so the output can be read as sql
func terminate(sql string) string {
	sql = strings.TrimSpace(sql)
	if strings.HasSuffix(sql, ";") {
		return sql
	}
	return sql + ";"
}

func plural(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
