package drift

import (
	"fmt"
	"io"
	"strings"
)

// Render writes the report as text: a summary line, then each difference with what the state files expect and what
// the database has
func Render(w io.Writer, r Report) {
	schema := "the schema"
	if r.Schema != "" {
		schema = "schema " + r.Schema
	}
	version := "not managed by skima"
	if r.Deployed != "" {
		version = "deployed " + r.Deployed
	}
	switch len(r.Findings) {
	case 0:
		fmt.Fprintf(w, "No drift: %s (%s) matches its state files\n", schema, version)
	case 1:
		fmt.Fprintf(w, "Drift in %s (%s): 1 difference\n", schema, version)
	default:
		fmt.Fprintf(w, "Drift in %s (%s): %d differences\n", schema, version, len(r.Findings))
	}
	for _, f := range r.Findings {
		name := f.Name
		if f.Attribute != "" {
			name += " (" + f.Attribute + ")"
		}
		fmt.Fprintf(w, "  %-8s %-10s %s\n", f.Change, f.Object, name)
		if f.Expected != "" {
			fmt.Fprintf(w, "             expected: %s\n", indent(f.Expected))
		}
		if f.Actual != "" {
			fmt.Fprintf(w, "             actual:   %s\n", indent(f.Actual))
		}
	}
	if r.Ignored > 0 {
		fmt.Fprintf(w, "%d ignored (drift.ignore)\n", r.Ignored)
	}
	if len(r.Pending) > 0 {
		changesets := fmt.Sprintf("%d changesets newer than the deployed version are", len(r.Pending))
		if len(r.Pending) == 1 {
			changesets = "1 changeset newer than the deployed version is"
		}
		fmt.Fprintf(w, "WARNING: %s not applied (%s) - differences they make show as drift. Run skima plan to see them\n", changesets, strings.Join(r.Pending, ", "))
	}
}

// indent lines up the continuation lines of a multi-line definition, e.g. a view's sql, under its first line
func indent(definition string) string {
	lines := strings.Split(strings.TrimSpace(definition), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.Join(lines, "\n                       ")
}
