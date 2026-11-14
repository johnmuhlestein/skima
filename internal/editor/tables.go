package editor

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/johnmuhlestein/skima/internal/manifest"
)

// ColumnChanges are the column attributes to change - nil leaves an attribute as it is. Renaming is RenameColumn
type ColumnChanges struct {
	Datatype *string `json:"datatype,omitempty"`
	Nullable *bool   `json:"nullable,omitempty"`
	Default  *string `json:"default,omitempty"`
	Dfltfunc *bool   `json:"dfltfunc,omitempty"`
	Comment  *string `json:"comment,omitempty"`
}

func checkNewName(what string, name string) error {
	if !newNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %s %q - use lowercase letters, digits, _ or $, starting with a letter or _", ErrInvalidName, what, name)
	}
	return nil
}

// CreateTable adds a new table
func (e *Editor) CreateTable(table manifest.Table) (Result, error) {
	if err := checkNewName("table", table.Name); err != nil {
		return Result{}, err
	}
	return e.edit(table.Name, "", func(w *working) error {
		if _, ok := w.tables[table.Name]; ok {
			return fmt.Errorf("%w: table '%s'", ErrExists, table.Name)
		}
		created := table
		w.tables[table.Name] = &created
		return nil
	})
}

// DropTable removes a table. Foreign keys on other tables that reference it, and sequences its columns own, are
// removed too when cascade is true - otherwise the drop is rejected with a ReferencedError listing them
func (e *Editor) DropTable(name string, etag string, cascade bool) (Result, error) {
	return e.edit(name, etag, func(w *working) error {
		if _, err := w.table(name); err != nil {
			return err
		}
		var refs []string
		for _, other := range sortedNames(w.tables) {
			if other == name {
				continue
			}
			for _, fk := range w.tables[other].ForeignKeys {
				if fk.Reference.Table == name {
					refs = append(refs, fmt.Sprintf("foreign key %s.%s", other, fk.Name))
				}
			}
		}
		for _, seq := range sortedNames(w.sequences) {
			if owner, _, _ := strings.Cut(w.sequences[seq].Ownedby, "."); owner == name {
				refs = append(refs, fmt.Sprintf("sequence %s (owned by %s)", seq, w.sequences[seq].Ownedby))
			}
		}
		if len(refs) > 0 && !cascade {
			return &ReferencedError{What: fmt.Sprintf("table '%s'", name), References: refs}
		}
		for _, other := range sortedNames(w.tables) {
			if other == name {
				continue
			}
			t := w.tables[other]
			t.ForeignKeys = slices.DeleteFunc(t.ForeignKeys, func(fk manifest.ForeignKey) bool {
				if fk.Reference.Table == name {
					w.warn("dropped foreign key %s.%s, which referenced table '%s'", other, fk.Name, name)
					return true
				}
				return false
			})
		}
		for _, seq := range sortedNames(w.sequences) {
			if owner, _, _ := strings.Cut(w.sequences[seq].Ownedby, "."); owner == name {
				delete(w.sequences, seq)
				w.warn("dropped sequence %s, which table '%s' owned", seq, name)
			}
		}
		delete(w.tables, name)
		w.warnSqlMentions(name, "")
		return nil
	})
}

// SetTableComment sets the table's comment - an empty comment removes it
func (e *Editor) SetTableComment(name string, etag string, comment string) (Result, error) {
	return e.edit(name, etag, func(w *working) error {
		t, err := w.table(name)
		if err != nil {
			return err
		}
		t.Comment = comment
		return nil
	})
}

// AddColumn adds a column at the end of the table
func (e *Editor) AddColumn(table string, etag string, column manifest.Column) (Result, error) {
	if err := checkNewName("column", column.Name); err != nil {
		return Result{}, err
	}
	return e.edit(table, etag, func(w *working) error {
		t, err := w.table(table)
		if err != nil {
			return err
		}
		if columnIndex(t, column.Name) >= 0 {
			return fmt.Errorf("%w: column '%s.%s'", ErrExists, table, column.Name)
		}
		t.Columns = append(t.Columns, column)
		return nil
	})
}

// AlterColumn changes a column's attributes
func (e *Editor) AlterColumn(table string, etag string, column string, changes ColumnChanges) (Result, error) {
	return e.edit(table, etag, func(w *working) error {
		t, err := w.table(table)
		if err != nil {
			return err
		}
		i := columnIndex(t, column)
		if i < 0 {
			return fmt.Errorf("%w: column '%s.%s'", ErrNotFound, table, column)
		}
		c := &t.Columns[i]
		if changes.Datatype != nil {
			c.Datatype = *changes.Datatype
		}
		if changes.Nullable != nil {
			c.Nullable = *changes.Nullable
		}
		if changes.Default != nil {
			c.Default = *changes.Default
		}
		if changes.Dfltfunc != nil {
			c.Dfltfunc = *changes.Dfltfunc
		}
		if changes.Comment != nil {
			c.Comment = *changes.Comment
		}
		return nil
	})
}

// RenameColumn renames a column, and the column in everything that names it: the table's primary key, indexes,
// constraints and foreign keys, other tables' foreign keys that reference it, and the sequences it owns. SQL that
// mentions the old name (index, constraint and trigger DDL, views) can't be rewritten safely and is reported as a
// warning. The rename is reported in the result so the changeset renames rather than drops and adds the column
func (e *Editor) RenameColumn(table string, etag string, from string, to string) (Result, error) {
	if err := checkNewName("column", to); err != nil {
		return Result{}, err
	}
	return e.edit(table, etag, func(w *working) error {
		t, err := w.table(table)
		if err != nil {
			return err
		}
		i := columnIndex(t, from)
		if i < 0 {
			return fmt.Errorf("%w: column '%s.%s'", ErrNotFound, table, from)
		}
		if from == to {
			return nil
		}
		if columnIndex(t, to) >= 0 {
			return fmt.Errorf("%w: column '%s.%s'", ErrExists, table, to)
		}
		t.Columns[i].Name = to
		rename := func(names []string) {
			for j := range names {
				if names[j] == from {
					names[j] = to
				}
			}
		}
		rename(t.PrimaryKey)
		for j := range t.Indexes {
			rename(t.Indexes[j].Columns)
		}
		for j := range t.Constraints {
			rename(t.Constraints[j].Columns)
		}
		for j := range t.ForeignKeys {
			rename(t.ForeignKeys[j].Columns)
		}
		for _, other := range sortedNames(w.tables) {
			for j, fk := range w.tables[other].ForeignKeys {
				if fk.Reference.Table == table {
					rename(w.tables[other].ForeignKeys[j].Reference.Columns)
				}
			}
		}
		for _, seq := range sortedNames(w.sequences) {
			if w.sequences[seq].Ownedby == table+"."+from {
				w.sequences[seq].Ownedby = table + "." + to
			}
		}
		w.warnSqlMentions(table, from)
		w.result.Renames = append(w.result.Renames, Rename{Table: table, From: from, To: to})
		return nil
	})
}

// DropColumn removes a column. If anything uses it - the primary key, an index, constraint or foreign key on the
// table, another table's foreign key, or a sequence it owns - the drop is rejected with a ReferencedError listing
// them, unless cascade is true: then those are removed too, as postgres does
func (e *Editor) DropColumn(table string, etag string, column string, cascade bool) (Result, error) {
	return e.edit(table, etag, func(w *working) error {
		t, err := w.table(table)
		if err != nil {
			return err
		}
		i := columnIndex(t, column)
		if i < 0 {
			return fmt.Errorf("%w: column '%s.%s'", ErrNotFound, table, column)
		}
		var refs []string
		if slices.Contains(t.PrimaryKey, column) {
			refs = append(refs, "the primary key")
		}
		for _, idx := range t.Indexes {
			if slices.Contains(idx.Columns, column) {
				refs = append(refs, "index "+idx.Name)
			}
		}
		for _, cons := range t.Constraints {
			if slices.Contains(cons.Columns, column) {
				refs = append(refs, "constraint "+cons.Name)
			}
		}
		for _, fk := range t.ForeignKeys {
			if slices.Contains(fk.Columns, column) {
				refs = append(refs, "foreign key "+fk.Name)
			}
		}
		for _, other := range sortedNames(w.tables) {
			for _, fk := range w.tables[other].ForeignKeys {
				if fk.Reference.Table == table && slices.Contains(fk.Reference.Columns, column) {
					refs = append(refs, fmt.Sprintf("foreign key %s.%s", other, fk.Name))
				}
			}
		}
		for _, seq := range sortedNames(w.sequences) {
			if w.sequences[seq].Ownedby == table+"."+column {
				refs = append(refs, "sequence "+seq)
			}
		}
		if len(refs) > 0 && !cascade {
			return &ReferencedError{What: fmt.Sprintf("column '%s.%s'", table, column), References: refs}
		}
		if slices.Contains(t.PrimaryKey, column) {
			t.PrimaryKey = nil
		}
		t.Indexes = slices.DeleteFunc(t.Indexes, func(idx manifest.Index) bool { return slices.Contains(idx.Columns, column) })
		t.Constraints = slices.DeleteFunc(t.Constraints, func(c manifest.Constraint) bool { return slices.Contains(c.Columns, column) })
		t.ForeignKeys = slices.DeleteFunc(t.ForeignKeys, func(fk manifest.ForeignKey) bool { return slices.Contains(fk.Columns, column) })
		for _, other := range sortedNames(w.tables) {
			o := w.tables[other]
			o.ForeignKeys = slices.DeleteFunc(o.ForeignKeys, func(fk manifest.ForeignKey) bool {
				return fk.Reference.Table == table && slices.Contains(fk.Reference.Columns, column)
			})
		}
		for _, seq := range sortedNames(w.sequences) {
			if w.sequences[seq].Ownedby == table+"."+column {
				delete(w.sequences, seq)
			}
		}
		for _, ref := range refs {
			w.warn("dropped %s, which used column '%s.%s'", ref, table, column)
		}
		t.Columns = slices.Delete(t.Columns, i, i+1)
		w.warnSqlMentions(table, column)
		return nil
	})
}

// SetPrimaryKey sets the table's primary key columns
func (e *Editor) SetPrimaryKey(table string, etag string, columns []string) (Result, error) {
	return e.edit(table, etag, func(w *working) error {
		t, err := w.table(table)
		if err != nil {
			return err
		}
		for _, c := range t.Columns {
			if c.Primarykey && c.Generate != "" {
				return fmt.Errorf("%w: table '%s' has an identity primary key column, %s - the state build ignores primary-key for it", ErrExists, table, c.Name)
			}
		}
		t.PrimaryKey = slices.Clone(columns)
		return nil
	})
}

// DropPrimaryKey removes the table's primary key. Other tables' foreign keys that reference the primary key columns
// need it, so the drop is rejected with a ReferencedError unless cascade is true, which removes them too
func (e *Editor) DropPrimaryKey(table string, etag string, cascade bool) (Result, error) {
	return e.edit(table, etag, func(w *working) error {
		t, err := w.table(table)
		if err != nil {
			return err
		}
		if len(t.PrimaryKey) == 0 {
			return fmt.Errorf("%w: table '%s' has no primary key", ErrNotFound, table)
		}
		referencing := func(fk manifest.ForeignKey) bool {
			return fk.Reference.Table == table && slices.Equal(fk.Reference.Columns, t.PrimaryKey)
		}
		var refs []string
		for _, other := range sortedNames(w.tables) {
			for _, fk := range w.tables[other].ForeignKeys {
				if referencing(fk) {
					refs = append(refs, fmt.Sprintf("foreign key %s.%s", other, fk.Name))
				}
			}
		}
		if len(refs) > 0 && !cascade {
			return &ReferencedError{What: fmt.Sprintf("the primary key of table '%s'", table), References: refs}
		}
		for _, other := range sortedNames(w.tables) {
			o := w.tables[other]
			o.ForeignKeys = slices.DeleteFunc(o.ForeignKeys, referencing)
		}
		for _, ref := range refs {
			w.warn("dropped %s, which referenced the primary key of table '%s'", ref, table)
		}
		t.PrimaryKey = nil
		return nil
	})
}

// PutForeignKey adds a foreign key, or replaces the one with the same name
func (e *Editor) PutForeignKey(table string, etag string, fk manifest.ForeignKey) (Result, error) {
	return e.put(table, etag, "foreign key", fk.Name, func(t *manifest.Table) {
		t.ForeignKeys = putNamed(t.ForeignKeys, fk, func(f manifest.ForeignKey) string { return f.Name })
	})
}

// DropForeignKey removes a foreign key
func (e *Editor) DropForeignKey(table string, etag string, name string) (Result, error) {
	return e.drop(table, etag, "foreign key", name, func(t *manifest.Table) bool {
		var found bool
		t.ForeignKeys, found = dropNamed(t.ForeignKeys, name, func(f manifest.ForeignKey) string { return f.Name })
		return found
	})
}

// PutIndex adds an index, or replaces the one with the same name
func (e *Editor) PutIndex(table string, etag string, idx manifest.Index) (Result, error) {
	return e.put(table, etag, "index", idx.Name, func(t *manifest.Table) {
		t.Indexes = putNamed(t.Indexes, idx, func(i manifest.Index) string { return i.Name })
	})
}

// DropIndex removes an index
func (e *Editor) DropIndex(table string, etag string, name string) (Result, error) {
	return e.drop(table, etag, "index", name, func(t *manifest.Table) bool {
		var found bool
		t.Indexes, found = dropNamed(t.Indexes, name, func(i manifest.Index) string { return i.Name })
		return found
	})
}

// PutConstraint adds a constraint, or replaces the one with the same name
func (e *Editor) PutConstraint(table string, etag string, cons manifest.Constraint) (Result, error) {
	return e.put(table, etag, "constraint", cons.Name, func(t *manifest.Table) {
		t.Constraints = putNamed(t.Constraints, cons, func(c manifest.Constraint) string { return c.Name })
	})
}

// DropConstraint removes a constraint
func (e *Editor) DropConstraint(table string, etag string, name string) (Result, error) {
	return e.drop(table, etag, "constraint", name, func(t *manifest.Table) bool {
		var found bool
		t.Constraints, found = dropNamed(t.Constraints, name, func(c manifest.Constraint) string { return c.Name })
		return found
	})
}

// PutTrigger adds a trigger, or replaces the one with the same name
func (e *Editor) PutTrigger(table string, etag string, trg manifest.Trigger) (Result, error) {
	return e.put(table, etag, "trigger", trg.Name, func(t *manifest.Table) {
		t.Triggers = putNamed(t.Triggers, trg, func(tr manifest.Trigger) string { return tr.Name })
	})
}

// DropTrigger removes a trigger
func (e *Editor) DropTrigger(table string, etag string, name string) (Result, error) {
	return e.drop(table, etag, "trigger", name, func(t *manifest.Table) bool {
		var found bool
		t.Triggers, found = dropNamed(t.Triggers, name, func(tr manifest.Trigger) string { return tr.Name })
		return found
	})
}

func (e *Editor) put(table string, etag string, what string, name string, change func(t *manifest.Table)) (Result, error) {
	if !namePattern.MatchString(name) {
		return Result{}, fmt.Errorf("%w: %s %q", ErrInvalidName, what, name)
	}
	return e.edit(table, etag, func(w *working) error {
		t, err := w.table(table)
		if err != nil {
			return err
		}
		change(t)
		return nil
	})
}

func (e *Editor) drop(table string, etag string, what string, name string, change func(t *manifest.Table) bool) (Result, error) {
	return e.edit(table, etag, func(w *working) error {
		t, err := w.table(table)
		if err != nil {
			return err
		}
		if !change(t) {
			return fmt.Errorf("%w: %s '%s' on table '%s'", ErrNotFound, what, name, table)
		}
		return nil
	})
}

// putNamed replaces the item with the same name in place, or appends it
func putNamed[T any](items []T, item T, name func(T) string) []T {
	for i := range items {
		if name(items[i]) == name(item) {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}

// dropNamed removes the item with the name, reporting whether there was one
func dropNamed[T any](items []T, n string, name func(T) string) ([]T, bool) {
	before := len(items)
	items = slices.DeleteFunc(items, func(item T) bool { return name(item) == n })
	return items, len(items) < before
}

func columnIndex(t *manifest.Table, name string) int {
	return slices.IndexFunc(t.Columns, func(c manifest.Column) bool { return c.Name == name })
}

func sortedNames[T any](m map[string]T) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func mentions(sql string, word string) bool {
	return word != "" && regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(word)+`\b`).MatchString(sql)
}

// warnSqlMentions warns about SQL that names a table's column (or, with no column, the table) - index, constraint and
// trigger DDL on the table, and views and materialized views that name both. skima can't rewrite SQL safely, so it
// needs checking by hand
func (w *working) warnSqlMentions(table string, column string) {
	word := column
	if word == "" {
		word = table
	}
	if t, ok := w.tables[table]; ok {
		for _, idx := range t.Indexes {
			if mentions(idx.Ddl, word) {
				w.warn("index %s's ddl mentions '%s' - check it", idx.Name, word)
			}
		}
		for _, cons := range t.Constraints {
			if mentions(cons.Condition, word) {
				w.warn("constraint %s's condition mentions '%s' - check it", cons.Name, word)
			}
		}
		for _, trg := range t.Triggers {
			if mentions(trg.Ddl, word) {
				w.warn("trigger %s's ddl mentions '%s' - check it", trg.Name, word)
			}
		}
	}
	for _, path := range sortedNames(w.otherSql) {
		if mentions(w.otherSql[path], table) && (column == "" || mentions(w.otherSql[path], column)) {
			w.warn("%s's sql mentions '%s' - check it", path, strings.Trim(table+"."+column, "."))
		}
	}
}
