// Package drift compares a schema as it is in the database with the schema its state files describe. Both sides are
// read from the postgres catalog - the expected one from a throwaway build of the state files - so postgres has
// normalized every definition (type names, defaults, check constraints, view sql) the same way and they compare as text.
package drift

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Schema is the tables and views of a schema, as the postgres catalog describes them
type Schema struct {
	Tables map[string]*Table
	Views  map[string]View
}

// View is a view's definition (pg_get_viewdef) and comment
type View struct {
	Definition string
	Comment    string
}

// Table is a table's columns and the objects defined on it. Constraints, indexes and triggers map a name to its
// definition as postgres prints it, without the schema name. Indexes leave out those that back a primary key, unique
// or exclusion constraint - they are compared as the constraint
type Table struct {
	Comment     string
	Columns     map[string]Column
	Constraints map[string]string
	Indexes     map[string]string
	Triggers    map[string]string
}

// Column is a column's definition - column order is not compared, since a column added by a changeset comes after
// the columns a state build creates before it
type Column struct {
	Type    string // format_type, e.g. "character varying(100)"
	NotNull bool
	Default string // pg_get_expr of the default, empty when there is none
	// Identity is "always" or "by default" for an identity column, otherwise empty
	Identity string
	// Generated is "stored" for a generated column, otherwise empty
	Generated string
	Comment   string
}

// NewSchema returns an empty schema
func NewSchema() Schema {
	return Schema{Tables: map[string]*Table{}, Views: map[string]View{}}
}

// Table returns the named table, adding it when the schema does not have it yet
func (s Schema) Table(name string) *Table {
	t, ok := s.Tables[name]
	if !ok {
		t = &Table{Columns: map[string]Column{}, Constraints: map[string]string{}, Indexes: map[string]string{}, Triggers: map[string]string{}}
		s.Tables[name] = t
	}
	return t
}

// String describes the column, e.g. "integer not null default 0"
func (c Column) String() string {
	parts := []string{c.Type}
	if c.NotNull {
		parts = append(parts, "not null")
	}
	if c.Default != "" {
		parts = append(parts, "default "+c.Default)
	}
	if c.Identity != "" {
		parts = append(parts, "generated "+c.Identity+" as identity")
	}
	if c.Generated != "" {
		parts = append(parts, "generated "+c.Generated)
	}
	return strings.Join(parts, " ")
}

// Kinds of difference
const (
	Missing = "missing" // the state files define it, the database does not have it
	Extra   = "extra"   // the database has it, the state files do not define it
	Changed = "changed" // both have it, with different definitions
)

// Finding is one difference between the schema in the database and the schema its state files describe
type Finding struct {
	Change string `json:"change"` // missing, extra or changed
	Object string `json:"object"` // table, column, constraint, index, trigger or view
	// Name is the object's name - qualified by its table for columns, constraints, indexes and triggers
	Name string `json:"name"`
	// Attribute is the part of a changed object that differs: a column's type, nullable, default, identity or
	// generated, or the comment of a table, column or view
	Attribute string `json:"attribute,omitempty"`
	Expected  string `json:"expected,omitempty"`
	Actual    string `json:"actual,omitempty"`
}

// table is the table a finding belongs to - its name, for a table or view
func (f Finding) table() string {
	if f.Object == "table" || f.Object == "view" {
		return f.Name
	}
	table, _, _ := strings.Cut(f.Name, ".")
	return table
}

// Compare returns the differences between the expected schema (from the state files) and the actual one, ordered by
// table or view, then object, then name
func Compare(expected Schema, actual Schema) []Finding {
	findings := []Finding{}
	for name, exp := range expected.Tables {
		act, ok := actual.Tables[name]
		if !ok {
			findings = append(findings, Finding{Change: Missing, Object: "table", Name: name})
			continue
		}
		if exp.Comment != act.Comment {
			findings = append(findings, commentFinding("table", name, exp.Comment, act.Comment))
		}
		findings = append(findings, compareColumns(name, exp.Columns, act.Columns)...)
		findings = append(findings, compareDefinitions("constraint", name, exp.Constraints, act.Constraints)...)
		findings = append(findings, compareDefinitions("index", name, exp.Indexes, act.Indexes)...)
		findings = append(findings, compareDefinitions("trigger", name, exp.Triggers, act.Triggers)...)
	}
	for name := range actual.Tables {
		if _, ok := expected.Tables[name]; !ok {
			findings = append(findings, Finding{Change: Extra, Object: "table", Name: name})
		}
	}
	findings = append(findings, compareViews(expected.Views, actual.Views)...)

	objectOrder := map[string]int{"table": 0, "view": 0, "column": 1, "constraint": 2, "index": 3, "trigger": 4}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.table() != b.table() {
			return a.table() < b.table()
		}
		if objectOrder[a.Object] != objectOrder[b.Object] {
			return objectOrder[a.Object] < objectOrder[b.Object]
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Attribute < b.Attribute
	})
	return findings
}

func compareColumns(table string, expected map[string]Column, actual map[string]Column) []Finding {
	var findings []Finding
	for name, exp := range expected {
		qualified := table + "." + name
		act, ok := actual[name]
		if !ok {
			findings = append(findings, Finding{Change: Missing, Object: "column", Name: qualified, Expected: exp.String()})
			continue
		}
		changed := func(attribute string, expected string, actual string) {
			if expected != actual {
				findings = append(findings, Finding{Change: Changed, Object: "column", Name: qualified, Attribute: attribute, Expected: expected, Actual: actual})
			}
		}
		changed("type", exp.Type, act.Type)
		changed("nullable", nullability(exp.NotNull), nullability(act.NotNull))
		changed("default", orNone(exp.Default), orNone(act.Default))
		changed("identity", orNone(exp.Identity), orNone(act.Identity))
		changed("generated", orNone(exp.Generated), orNone(act.Generated))
		changed("comment", orNone(exp.Comment), orNone(act.Comment))
	}
	for name, act := range actual {
		if _, ok := expected[name]; !ok {
			findings = append(findings, Finding{Change: Extra, Object: "column", Name: table + "." + name, Actual: act.String()})
		}
	}
	return findings
}

// compareDefinitions compares objects that are a name and a definition - table is empty for views, which are not on
// a table
func compareDefinitions(object string, table string, expected map[string]string, actual map[string]string) []Finding {
	qualify := func(name string) string {
		if table == "" {
			return name
		}
		return table + "." + name
	}
	var findings []Finding
	for name, exp := range expected {
		act, ok := actual[name]
		switch {
		case !ok:
			findings = append(findings, Finding{Change: Missing, Object: object, Name: qualify(name), Expected: exp})
		case act != exp:
			findings = append(findings, Finding{Change: Changed, Object: object, Name: qualify(name), Expected: exp, Actual: act})
		}
	}
	for name, act := range actual {
		if _, ok := expected[name]; !ok {
			findings = append(findings, Finding{Change: Extra, Object: object, Name: qualify(name), Actual: act})
		}
	}
	return findings
}

func compareViews(expected map[string]View, actual map[string]View) []Finding {
	definitions := func(views map[string]View) map[string]string {
		defs := map[string]string{}
		for name, view := range views {
			defs[name] = view.Definition
		}
		return defs
	}
	findings := compareDefinitions("view", "", definitions(expected), definitions(actual))
	for name, exp := range expected {
		if act, ok := actual[name]; ok && exp.Comment != act.Comment {
			findings = append(findings, commentFinding("view", name, exp.Comment, act.Comment))
		}
	}
	return findings
}

func commentFinding(object string, name string, expected string, actual string) Finding {
	return Finding{Change: Changed, Object: object, Name: name, Attribute: "comment", Expected: orNone(expected), Actual: orNone(actual)}
}

func nullability(notNull bool) string {
	if notNull {
		return "not null"
	}
	return "null"
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

// Report is the outcome of a drift check
type Report struct {
	Schema string `json:"schema,omitempty"`
	// Deployed is the version skima last applied - empty when skima does not manage the schema
	Deployed string `json:"deployed"`
	// Pending lists the changesets newer than the deployed version - the differences they make show as drift
	Pending  []string  `json:"pending,omitempty"`
	Findings []Finding `json:"findings"`
	// Ignored counts the differences left out by the ignore patterns
	Ignored int `json:"ignored"`
}

// Drifted reports whether the schema differs from its state files
func (r Report) Drifted() bool {
	return len(r.Findings) > 0
}

// Ignore leaves out the findings matching any of the patterns. A pattern is matched (path.Match syntax, e.g. "legacy_*"
// or "documents.*_idx") against the finding's name, and against its table or view - so a table's name ignores everything
// on it too
func (r *Report) Ignore(patterns []string) error {
	for _, pattern := range patterns {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid ignore pattern %q: %w", pattern, err)
		}
	}
	kept := r.Findings[:0]
	for _, f := range r.Findings {
		if ignored(f, patterns) {
			r.Ignored++
		} else {
			kept = append(kept, f)
		}
	}
	r.Findings = kept
	return nil
}

func ignored(f Finding, patterns []string) bool {
	for _, pattern := range patterns {
		for _, name := range []string{f.Name, f.table()} {
			if matched, _ := path.Match(pattern, name); matched {
				return true
			}
		}
	}
	return false
}
