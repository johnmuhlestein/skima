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
	Tables    map[string]*Table
	Views     map[string]View
	Enums     map[string]Enum
	Sequences map[string]Sequence
	Matviews  map[string]*Matview
}

// Matview is a materialized view's definition (pg_get_viewdef), comment and indexes - indexes map a name to its
// definition, without the schema name
type Matview struct {
	Definition string
	Comment    string
	Indexes    map[string]string
}

// Matview returns the named materialized view, adding it when the schema does not have it yet
func (s Schema) Matview(name string) *Matview {
	mv, ok := s.Matviews[name]
	if !ok {
		mv = &Matview{Indexes: map[string]string{}}
		s.Matviews[name] = mv
	}
	return mv
}

// Sequence is a sequence's settings, owner and comment - sequences of identity columns are compared as the column
type Sequence struct {
	Type      string // format_type of the sequence's data type, e.g. "bigint"
	Start     int64
	Increment int64
	Min       int64
	Max       int64
	Cache     int64
	Cycle     bool
	// OwnedBy is the table.column that owns the sequence, empty when no column does
	OwnedBy string
	Comment string
}

// Enum is an enum type's values, in sort order, and comment
type Enum struct {
	Values  []string
	Comment string
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
	return Schema{Tables: map[string]*Table{}, Views: map[string]View{}, Enums: map[string]Enum{}, Sequences: map[string]Sequence{},
		Matviews: map[string]*Matview{}}
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
	Object string `json:"object"` // table, column, constraint, index, trigger, view, matview, enum or sequence
	// Name is the object's name - qualified by its table (or materialized view) for columns, constraints, indexes and
	// triggers
	Name string `json:"name"`
	// Attribute is the part of a changed object that differs: a column's type, nullable, default, identity or
	// generated, a sequence's type, start, increment, minvalue, maxvalue, cache, cycle or owned by, or the comment of a
	// table, column, view, materialized view, enum or sequence
	Attribute string `json:"attribute,omitempty"`
	Expected  string `json:"expected,omitempty"`
	Actual    string `json:"actual,omitempty"`
}

// table is the table a finding belongs to - its name, for a table, view, materialized view, enum or sequence
func (f Finding) table() string {
	switch f.Object {
	case "table", "view", "matview", "enum", "sequence":
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
	findings = append(findings, compareEnums(expected.Enums, actual.Enums)...)
	findings = append(findings, compareSequences(expected.Sequences, actual.Sequences)...)
	findings = append(findings, compareMatviews(expected.Matviews, actual.Matviews)...)

	objectOrder := map[string]int{"table": 0, "view": 0, "matview": 0, "enum": 0, "sequence": 0, "column": 1, "constraint": 2, "index": 3, "trigger": 4}
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

// compareEnums compares enum types by their values in order - postgres sorts an enum by the order its values were
// added, so a value in the wrong position is a difference too
func compareEnums(expected map[string]Enum, actual map[string]Enum) []Finding {
	values := func(enums map[string]Enum) map[string]string {
		described := map[string]string{}
		for name, enum := range enums {
			described[name] = strings.Join(enum.Values, ", ")
		}
		return described
	}
	findings := compareDefinitions("enum", "", values(expected), values(actual))
	for name, exp := range expected {
		if act, ok := actual[name]; ok && exp.Comment != act.Comment {
			findings = append(findings, commentFinding("enum", name, exp.Comment, act.Comment))
		}
	}
	return findings
}

// compareMatviews compares materialized views by their definition, comment and indexes
func compareMatviews(expected map[string]*Matview, actual map[string]*Matview) []Finding {
	definitions := func(matviews map[string]*Matview) map[string]string {
		defs := map[string]string{}
		for name, mv := range matviews {
			defs[name] = mv.Definition
		}
		return defs
	}
	findings := compareDefinitions("matview", "", definitions(expected), definitions(actual))
	for name, exp := range expected {
		act, ok := actual[name]
		if !ok {
			continue
		}
		if exp.Comment != act.Comment {
			findings = append(findings, commentFinding("matview", name, exp.Comment, act.Comment))
		}
		findings = append(findings, compareDefinitions("index", name, exp.Indexes, act.Indexes)...)
	}
	return findings
}

// compareSequences compares sequences setting by setting. A sequence the state files do not define is extra only when
// no column owns it - an owned sequence (e.g. a serial column's) belongs to its column, which is compared instead
func compareSequences(expected map[string]Sequence, actual map[string]Sequence) []Finding {
	var findings []Finding
	for name, exp := range expected {
		act, ok := actual[name]
		if !ok {
			findings = append(findings, Finding{Change: Missing, Object: "sequence", Name: name, Expected: exp.String()})
			continue
		}
		changed := func(attribute string, expected string, actual string) {
			if expected != actual {
				findings = append(findings, Finding{Change: Changed, Object: "sequence", Name: name, Attribute: attribute, Expected: expected, Actual: actual})
			}
		}
		number := func(n int64) string { return fmt.Sprint(n) }
		changed("type", exp.Type, act.Type)
		changed("start", number(exp.Start), number(act.Start))
		changed("increment", number(exp.Increment), number(act.Increment))
		changed("minvalue", number(exp.Min), number(act.Min))
		changed("maxvalue", number(exp.Max), number(act.Max))
		changed("cache", number(exp.Cache), number(act.Cache))
		changed("cycle", fmt.Sprint(exp.Cycle), fmt.Sprint(act.Cycle))
		changed("owned by", orNone(exp.OwnedBy), orNone(act.OwnedBy))
		changed("comment", orNone(exp.Comment), orNone(act.Comment))
	}
	for name, act := range actual {
		if _, ok := expected[name]; !ok && act.OwnedBy == "" {
			findings = append(findings, Finding{Change: Extra, Object: "sequence", Name: name, Actual: act.String()})
		}
	}
	return findings
}

// String describes the sequence, e.g. "bigint start 1000 increment 5 owned by tags.seq_no"
func (s Sequence) String() string {
	described := fmt.Sprintf("%s start %d increment %d minvalue %d maxvalue %d cache %d", s.Type, s.Start, s.Increment, s.Min, s.Max, s.Cache)
	if s.Cycle {
		described += " cycle"
	}
	if s.OwnedBy != "" {
		described += " owned by " + s.OwnedBy
	}
	return described
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
