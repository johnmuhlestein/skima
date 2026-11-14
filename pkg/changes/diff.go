package changes

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"skima/pkg/manifest"
	"slices"
	"sort"
	"strings"
)

// Change is one difference between the baseline and the current state files
type Change struct {
	// Action is added, removed, changed or renamed
	Action string `json:"action"`
	// Object is what changed: table, column, primary key, foreign key, index, constraint, trigger, comment, view,
	// enum, materialized view, sequence or sql file
	Object string `json:"object"`
	// Target names it, e.g. documents.status
	Target string `json:"target"`
	Detail string `json:"detail,omitempty"`
}

func (c Change) String() string {
	s := fmt.Sprintf("%s %s %s", c.Object, c.Target, c.Action)
	if c.Detail != "" {
		s += " - " + c.Detail
	}
	return s
}

// Result is how the state files changed since the baseline, and the deltas that make the supported changes
type Result struct {
	Baseline Baseline `json:"baseline"`
	// PendingChangesets are changeset files not committed yet - the baseline only moves when they are
	PendingChangesets []string `json:"pendingChangesets,omitempty"`
	Changes           []Change `json:"changes"`
	// Unsupported are changes no delta is generated for - they need deltas written by hand
	Unsupported []Change `json:"unsupported,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Deltas      []Delta  `json:"deltas"`
}

// HasChanges reports whether the state files changed since the baseline
func (r Result) HasChanges() bool {
	return len(r.Changes) > 0 || len(r.Unsupported) > 0
}

// Diff compares the state files in the working directory with the baseline's
func Diff(workdir string, baseline Baseline, draft Draft) (Result, error) {
	base, err := ReadBaselineState(workdir, baseline)
	if err != nil {
		return Result{}, err
	}
	current, err := ReadWorkingState(workdir)
	if err != nil {
		return Result{}, err
	}
	result := Compare(base, current, draft)
	result.Baseline = baseline
	if result.PendingChangesets, err = PendingChangesets(workdir); err != nil {
		return Result{}, err
	}
	return result, nil
}

type generator struct {
	result    Result
	backfills map[string]*BackfillHint
	used      map[string]bool
	current   State
	// base is the baseline's tables with the rename hints applied
	base map[string]manifest.Table
}

// warnViews warns that postgres refuses a change while a view or materialized view depends on the table (or its
// column): the view has to be dropped before the change and added after it, by hand while views generate no deltas
func (g *generator) warnViews(table string, column string, change string) {
	for _, key := range sortedKeys(g.current.Other) {
		kind, name, _ := strings.Cut(key, " ")
		if kind != "view" && kind != "matview" {
			continue
		}
		definition, _ := g.current.Other[key].(map[string]any)
		sql, _ := definition["sql"].(string)
		if mentions(sql, table) && (column == "" || mentions(sql, column)) {
			label := map[string]string{"view": "view", "matview": "materialized view"}[kind]
			g.warn("%s fails while %s %s uses it - drop %s before it and add it back after (write those deltas by hand)", change, label, name, name)
		}
	}
}

func mentions(sql string, word string) bool {
	return word != "" && regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(word)+`\b`).MatchString(sql)
}

func (g *generator) change(action string, object string, target string, detail string) {
	g.result.Changes = append(g.result.Changes, Change{Action: action, Object: object, Target: target, Detail: detail})
}

func (g *generator) unsupported(action string, object string, target string, detail string) {
	g.result.Unsupported = append(g.result.Unsupported, Change{Action: action, Object: object, Target: target, Detail: detail})
}

func (g *generator) warn(format string, args ...any) {
	g.result.Warnings = append(g.result.Warnings, fmt.Sprintf(format, args...))
}

func (g *generator) delta(phase int, d Delta) {
	d.phase = phase
	g.result.Deltas = append(g.result.Deltas, d)
}

// Compare works out how the state changed from base to current, and the deltas that make the changes. Rename hints
// that still match the files are used; the rest are ignored with a warning
func Compare(base State, current State, draft Draft) Result {
	g := &generator{result: Result{Changes: []Change{}, Deltas: []Delta{}}, backfills: map[string]*BackfillHint{}, used: map[string]bool{}, current: current}
	for i := range draft.Backfills {
		b := &draft.Backfills[i]
		g.backfills[b.Table+"."+b.Column] = b
	}

	renames := map[string][]RenameHint{}
	for _, r := range draft.Renames {
		b, inBase := base.Tables[r.Table]
		c, inCurrent := current.Tables[r.Table]
		if inBase && inCurrent && hasColumn(b, r.From) && !hasColumn(c, r.From) && hasColumn(c, r.To) && !hasColumn(b, r.To) {
			renames[r.Table] = append(renames[r.Table], r)
		} else {
			g.warn("the rename of %s.%s to %s no longer matches the state files - ignored", r.Table, r.From, r.To)
		}
	}
	renamedBase := applyRenames(base.Tables, renames)
	g.base = renamedBase

	for _, name := range sortedKeys(current.Tables) {
		if _, ok := base.Tables[name]; !ok {
			g.change("added", "table", name, "")
			g.delta(phaseAddTables, Delta{Object: "table", Action: "add", Name: name})
		}
	}
	for _, name := range sortedKeys(base.Tables) {
		if _, ok := current.Tables[name]; !ok {
			g.change("removed", "table", name, "")
			g.delta(phaseDropTables, Delta{Object: "table", Action: "drop", Name: name})
			g.warn("dropping table %s deletes its data", name)
			g.warnViews(name, "", "dropping table "+name)
			for _, added := range sortedKeys(current.Tables) {
				if _, existed := base.Tables[added]; !existed && sameColumns(base.Tables[name], current.Tables[added]) {
					g.warn("table %s was removed and %s added with the same columns - if it is a rename, skima has no table rename delta: write a script delta (alter table %s rename to %s) instead of the drop and add", name, added, name, added)
				}
			}
		}
	}
	for _, name := range sortedKeys(current.Tables) {
		if _, ok := base.Tables[name]; ok {
			g.compareTable(name, renamedBase[name], current.Tables[name], renames[name])
		}
	}
	for _, key := range sortedKeys(g.backfills) {
		if !g.used[key] {
			g.warn("the backfill for %s is not needed - it is only used for a new NOT NULL column with no default on an existing table - ignored", key)
		}
	}

	g.compareOthers(base, current, renames)
	sort.SliceStable(g.result.Deltas, func(i, j int) bool { return g.result.Deltas[i].phase < g.result.Deltas[j].phase })
	return g.result
}

func (g *generator) compareTable(name string, base manifest.Table, current manifest.Table, renames []RenameHint) {
	for _, r := range renames {
		g.change("renamed", "column", name+"."+r.From, "to "+r.To)
		g.delta(phaseRenameColumns, Delta{Object: "column", Action: "rename", Name: r.From, Reference: name, NewName: r.To})
	}

	var added []manifest.Column
	for _, c := range current.Columns {
		if !hasColumn(base, c.Name) {
			added = append(added, c)
		}
	}
	for _, c := range added {
		target := name + "." + c.Name
		if c.Primarykey && c.Generate != "" {
			g.unsupported("added", "column", target, "identity columns can't be added to an existing table by a generated delta - write it by hand")
			continue
		}
		g.change("added", "column", target, describeColumn(c))
		backfill, hasBackfill := g.backfills[target]
		g.used[target] = hasBackfill
		needsBackfill := !c.Nullable && c.Default == ""
		if needsBackfill && hasBackfill {
			// add it nullable, fill it, then make it NOT NULL
			def := columnDef(c)
			def.Nullable = true
			g.delta(phaseAddColumns, Delta{Object: "column", Action: "add", Name: c.Name, Reference: name, Def: def})
			fill := Delta{Object: "script", Description: "backfill " + target}
			if backfill.Script != "" {
				fill.Script = backfill.Script
			} else {
				fill.Sql = []string{fmt.Sprintf("update %s set %s = %s where %s is null", name, c.Name, backfill.Expression, c.Name)}
			}
			g.delta(phaseAddColumns, fill)
			g.delta(phaseAddColumns, Delta{Object: "column", Action: "alter", Name: c.Name, Reference: name, Mods: &Mods{Nullable: true}})
		} else {
			g.delta(phaseAddColumns, Delta{Object: "column", Action: "add", Name: c.Name, Reference: name})
			if needsBackfill {
				g.warn("%s is NOT NULL with no default - adding it fails if %s has rows: give it a backfill or a default", target, name)
			} else if hasBackfill {
				g.used[target] = false // reported as not needed
			}
		}
	}

	var removed []manifest.Column
	for _, c := range base.Columns {
		if !hasColumn(current, c.Name) {
			removed = append(removed, c)
			g.change("removed", "column", name+"."+c.Name, "")
			g.delta(phaseDropColumns, Delta{Object: "column", Action: "drop", Name: c.Name, Reference: name})
			g.warn("dropping column %s.%s deletes its data", name, c.Name)
			g.warnViews(name, c.Name, "dropping column "+name+"."+c.Name)
		}
	}
	for _, r := range removed {
		for _, a := range added {
			if strings.EqualFold(r.Datatype, a.Datatype) {
				g.warn("%s.%s was removed and %s.%s added with the same datatype - if it is a rename, record it (skima changeset rename %s %s %s), or the data in %s is lost", name, r.Name, name, a.Name, name, r.Name, a.Name, r.Name)
			}
		}
	}

	for _, c := range current.Columns {
		i := slices.IndexFunc(base.Columns, func(b manifest.Column) bool { return b.Name == c.Name })
		if i < 0 {
			continue
		}
		b, target := base.Columns[i], name+"."+c.Name
		if b.Primarykey != c.Primarykey || b.Generate != c.Generate {
			g.unsupported("changed", "column", target, "turning primarykey or generate (identity) on or off for an existing column isn't generated - write it by hand")
		}
		if !strings.EqualFold(b.Datatype, c.Datatype) {
			g.change("changed", "column", target, fmt.Sprintf("datatype %s to %s", b.Datatype, c.Datatype))
			g.delta(phaseAlterColumns, Delta{Object: "column", Action: "alter", Name: c.Name, Reference: name, Mods: &Mods{Datatype: true}})
			g.warn("changing %s to %s rewrites %s, and fails if postgres can't cast the existing values", target, c.Datatype, name)
			g.warnViews(name, c.Name, "changing the datatype of "+target)
		}
		if b.Default != c.Default || b.Dfltfunc != c.Dfltfunc {
			g.change("changed", "column", target, fmt.Sprintf("default %s to %s", orNone(b.Default), orNone(c.Default)))
			g.delta(phaseAlterColumns, Delta{Object: "column", Action: "alter", Name: c.Name, Reference: name, Mods: &Mods{Default: true}})
		}
		if b.Nullable != c.Nullable {
			detail := "now nullable"
			if !c.Nullable {
				detail = "now NOT NULL"
				g.warn("making %s NOT NULL fails if it has nulls", target)
			}
			g.change("changed", "column", target, detail)
			g.delta(phaseAlterColumns, Delta{Object: "column", Action: "alter", Name: c.Name, Reference: name, Mods: &Mods{Nullable: true}})
		}
		if b.Comment != c.Comment {
			g.comment("column", c.Name, name, b.Comment, c.Comment)
		}
	}

	// an identity column that is the primary key replaces the table's primary-key - turning one on or off is reported
	// as unsupported with the column, above
	basePK, currentPK := effectivePrimaryKey(base), effectivePrimaryKey(current)
	if !slices.Equal(basePK, currentPK) && !hasIdentityPrimaryKey(base) && !hasIdentityPrimaryKey(current) {
		pkName := name + "_pk" // the name the state build gives it
		if len(basePK) > 0 {
			g.delta(phaseDropDependents, Delta{Object: "pk", Action: "drop", Name: pkName, Reference: name})
			g.rebuildDependentForeignKeys(name, basePK)
		}
		if len(currentPK) > 0 {
			g.delta(phaseAddDependents, Delta{Object: "pk", Action: "add", Name: pkName, Reference: name})
		}
		g.change(changeAction(len(basePK) > 0, len(currentPK) > 0), "primary key", name,
			fmt.Sprintf("(%s) to (%s)", strings.Join(basePK, ", "), strings.Join(currentPK, ", ")))
	}

	compareNamed(g, name, "foreign key", "fk", base.ForeignKeys, current.ForeignKeys, func(f manifest.ForeignKey) string { return f.Name }, phaseAddForeignKeys, true)
	compareNamed(g, name, "index", "index", base.Indexes, current.Indexes, func(i manifest.Index) string { return i.Name }, phaseAddDependents, false)
	compareNamed(g, name, "constraint", "constraint", base.Constraints, current.Constraints, func(c manifest.Constraint) string { return c.Name }, phaseAddDependents, true)
	compareNamed(g, name, "trigger", "trigger", base.Triggers, current.Triggers, func(t manifest.Trigger) string { return t.Name }, phaseAddDependents, true)

	if base.Comment != current.Comment {
		g.comment("table", name, "", base.Comment, current.Comment)
	}
}

// rebuildDependentForeignKeys drops the foreign keys that reference a table's primary key before it is dropped, and
// adds them back once the new one exists - postgres refuses to drop a primary key that foreign keys depend on. A
// foreign key that changed or was removed anyway is left to its own deltas
func (g *generator) rebuildDependentForeignKeys(table string, primaryKey []string) {
	for _, other := range sortedKeys(g.base) {
		current, stillThere := g.current.Tables[other]
		for _, fk := range g.base[other].ForeignKeys {
			if fk.Reference.Table != table || !slices.Equal(fk.Reference.Columns, primaryKey) || !stillThere {
				continue
			}
			i := slices.IndexFunc(current.ForeignKeys, func(c manifest.ForeignKey) bool { return c.Name == fk.Name })
			if i < 0 || !reflect.DeepEqual(current.ForeignKeys[i], fk) {
				continue
			}
			g.delta(phaseDropForeignKeys, Delta{Object: "fk", Action: "drop", Name: fk.Name, Reference: other})
			g.delta(phaseAddForeignKeys, Delta{Object: "fk", Action: "add", Name: fk.Name, Reference: other})
			g.warn("foreign key %s.%s depends on the primary key of %s - it is dropped before the primary key changes and added back after", other, fk.Name, table)
		}
	}
}

// effectivePrimaryKey is the primary key the state build creates: an identity column marked primarykey, or else the
// table's primary-key
func effectivePrimaryKey(t manifest.Table) []string {
	for _, c := range t.Columns {
		if c.Primarykey && c.Generate != "" {
			return []string{c.Name}
		}
	}
	return t.PrimaryKey
}

func hasIdentityPrimaryKey(t manifest.Table) bool {
	return slices.ContainsFunc(t.Columns, func(c manifest.Column) bool { return c.Primarykey && c.Generate != "" })
}

// compareNamed compares a table's named objects of one kind: a removed one is dropped, an added one added, and a
// changed one dropped and added again. dropNeedsTable is whether the drop delta names the table (an index drop doesn't)
func compareNamed[T any](g *generator, table string, what string, object string, base []T, current []T, name func(T) string, addPhase int, dropNeedsTable bool) {
	byName := func(items []T) map[string]T {
		m := map[string]T{}
		for _, item := range items {
			m[name(item)] = item
		}
		return m
	}
	b, c := byName(base), byName(current)
	drop := func(n string) {
		d := Delta{Object: object, Action: "drop", Name: n}
		if dropNeedsTable {
			d.Reference = table
		}
		phase := phaseDropDependents
		if object == "fk" {
			phase = phaseDropForeignKeys
		}
		g.delta(phase, d)
	}
	add := func(n string) {
		g.delta(addPhase, Delta{Object: object, Action: "add", Name: n, Reference: table})
	}
	for _, n := range sortedKeys(b) {
		if _, ok := c[n]; !ok {
			g.change("removed", what, table+"."+n, "")
			drop(n)
		}
	}
	for _, n := range sortedKeys(c) {
		old, existed := b[n]
		switch {
		case !existed:
			g.change("added", what, table+"."+n, "")
			add(n)
		case !reflect.DeepEqual(old, c[n]):
			g.change("changed", what, table+"."+n, "dropped and added again")
			drop(n)
			add(n)
		}
	}
}

// comment generates the delta for a changed table or column comment - a column comment names its table in reference
func (g *generator) comment(on string, name string, table string, before string, after string) {
	target := name
	if table != "" {
		target = table + "." + name
	}
	action := "add"
	if after == "" {
		action = "drop"
	}
	g.change(changeAction(before != "", after != ""), "comment", on+" "+target, "")
	g.delta(phaseComments, Delta{Object: "comment", Action: action, On: on, Name: name, Reference: table})
}

// compareOthers reports changes to the state no deltas are generated for: views, enums, materialized views,
// sequences and state sql files
func (g *generator) compareOthers(base State, current State, renames map[string][]RenameHint) {
	label := map[string]string{"view": "view", "enum": "enum", "matview": "materialized view"}
	for _, key := range unionKeys(base.Other, current.Other) {
		kind, name, _ := strings.Cut(key, " ")
		b, inBase := base.Other[key]
		c, inCurrent := current.Other[key]
		if action, ok := otherChange(inBase, inCurrent, reflect.DeepEqual(b, c)); ok {
			g.unsupported(action, label[kind], name, "no delta is generated for it yet - write it by hand")
		}
	}
	for _, name := range unionKeys(base.Sequences, current.Sequences) {
		b, inBase := base.Sequences[name]
		c, inCurrent := current.Sequences[name]
		if inBase && inCurrent && b.Ownedby != c.Ownedby && ownerRenamed(b.Ownedby, c.Ownedby, renames) {
			b.Ownedby = c.Ownedby // the column was renamed - postgres moves the ownership with it
		}
		if action, ok := otherChange(inBase, inCurrent, reflect.DeepEqual(b, c)); ok {
			g.unsupported(action, "sequence", name, "no delta is generated for it yet - write it by hand")
		}
	}
	for _, path := range unionKeys(base.Sql, current.Sql) {
		b, inBase := base.Sql[path]
		c, inCurrent := current.Sql[path]
		if action, ok := otherChange(inBase, inCurrent, b == c); ok {
			g.unsupported(action, "sql file", path, "state sql files don't generate deltas - if databases need the change, add a function or script delta by hand")
		}
	}
}

func otherChange(inBase bool, inCurrent bool, equal bool) (string, bool) {
	switch {
	case inCurrent && !inBase:
		return "added", true
	case inBase && !inCurrent:
		return "removed", true
	case !equal:
		return "changed", true
	}
	return "", false
}

func ownerRenamed(before string, after string, renames map[string][]RenameHint) bool {
	table, column, _ := strings.Cut(before, ".")
	for _, r := range renames[table] {
		if r.From == column && after == table+"."+r.To {
			return true
		}
	}
	return false
}

// applyRenames returns a copy of the tables with the renamed columns renamed everywhere they are named - as postgres
// does when a column is renamed - so the rest of the comparison sees them under their new names
func applyRenames(tables map[string]manifest.Table, renames map[string][]RenameHint) map[string]manifest.Table {
	renamed := map[string]manifest.Table{}
	for name, t := range tables {
		raw, _ := json.Marshal(t)
		var clone manifest.Table
		_ = json.Unmarshal(raw, &clone)
		renamed[name] = clone
	}
	for table, hints := range renames {
		for _, r := range hints {
			rename := func(names []string) {
				for i := range names {
					if names[i] == r.From {
						names[i] = r.To
					}
				}
			}
			t := renamed[table]
			for i := range t.Columns {
				if t.Columns[i].Name == r.From {
					t.Columns[i].Name = r.To
				}
			}
			rename(t.PrimaryKey)
			for i := range t.Indexes {
				rename(t.Indexes[i].Columns)
			}
			for i := range t.Constraints {
				rename(t.Constraints[i].Columns)
			}
			for i := range t.ForeignKeys {
				rename(t.ForeignKeys[i].Columns)
			}
			renamed[table] = t
			for _, other := range renamed {
				for i := range other.ForeignKeys {
					if other.ForeignKeys[i].Reference.Table == table {
						rename(other.ForeignKeys[i].Reference.Columns)
					}
				}
			}
		}
	}
	return renamed
}

func hasColumn(t manifest.Table, name string) bool {
	return slices.ContainsFunc(t.Columns, func(c manifest.Column) bool { return c.Name == name })
}

func sameColumns(a manifest.Table, b manifest.Table) bool {
	if len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i].Name != b.Columns[i].Name || !strings.EqualFold(a.Columns[i].Datatype, b.Columns[i].Datatype) {
			return false
		}
	}
	return true
}

func describeColumn(c manifest.Column) string {
	s := c.Datatype
	if !c.Nullable {
		s += " not null"
	}
	if c.Default != "" {
		s += " default " + c.Default
	}
	return s
}

func changeAction(before bool, after bool) string {
	switch {
	case !before:
		return "added"
	case !after:
		return "removed"
	}
	return "changed"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func unionKeys[T any](a map[string]T, b map[string]T) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	return sortedKeys(seen)
}
