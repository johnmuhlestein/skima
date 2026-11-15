// Package manifest reads the manifest's files - the state files (tables, views, enums, sequences, materialized views,
// sql files) and the changesets in deltas/ - validates and formats them, and generates the DDL they describe
package manifest

import (
	_ "embed"

	"github.com/johnmuhlestein/skima/internal/util"
)

// Delta is one change in a changeset
type Delta interface {
	// Ddl generates the delta's statement - a *MissingDefinitionError when its definition is not in the state files
	Ddl() (string, error)
	StatementType() string
	Describe() string
}

// DeltaHook is a delta that can run sql before and after it (its pre and post hooks)
type DeltaHook interface {
	HasPreHook() bool
	HasPostHook() bool
}

// PrimaryKeyRef is the table and columns a foreign key references
type PrimaryKeyRef struct {
	Table   string
	Columns []string
}

// ForeignKey is a table's foreign key - its columns, the table and columns they reference, and the referential actions
type ForeignKey struct {
	Name       string
	Columns    []string
	Reference  PrimaryKeyRef
	DeleteRule string
	UpdateRule string
}

// Index is a table's or materialized view's index - on columns, or a full create index statement (ddl)
type Index struct {
	Name    string
	Columns []string
	Unique  bool
	Ddl     string
}

// Column is a table column - its datatype, nullability, default, identity and comment
type Column struct {
	Name       string
	Datatype   string
	Nullable   bool
	Default    string
	Dfltfunc   bool
	Primarykey bool
	Generate   string
	Comment    string
}

// Trigger is a table trigger - its full create trigger statement
type Trigger struct {
	Name string
	Ddl  string
}

// Constraint is a table's unique or check constraint
type Constraint struct {
	Name      string
	Type      string
	Columns   []string
	Condition string
}

// Table is a table state file (state/<name>-table.json)
type Table struct {
	Name        string
	Columns     []Column
	PrimaryKey  []string     `json:"primary-key"`
	ForeignKeys []ForeignKey `json:"foreign-keys"`
	Indexes     []Index
	Triggers    []Trigger
	Constraints []Constraint
	Comment     string
}

// View is a view state file (state/<name>-view.json) - its columns and query
type View struct {
	Name    string
	Columns []string
	Sql     string
	Comment string
}

// Enum is an enum type - its values in sort order
type Enum struct {
	Name    string
	Values  []string
	Comment string
}

// Sequence is a sequence - the settings that are nil take postgres' defaults
type Sequence struct {
	Name      string
	Datatype  string
	Start     *int64
	Increment *int64
	Minvalue  *int64
	Maxvalue  *int64
	Cache     *int64
	Cycle     bool
	// Ownedby is the table.column that owns the sequence - it is dropped with the column
	Ownedby string
	Comment string
}

// Matview is a materialized view - its query, indexes and comment
type Matview struct {
	Name    string
	Columns []string
	Sql     string
	// Data is false to create it with no data - it is empty until refreshed
	Data    *bool
	Indexes []Index
	Comment string
}

// MatviewShadowPrefix names the view that stands in for a materialized view's query in a shadow build - see ShadowDdl
const MatviewShadowPrefix = "skima_matview_"

// Hook is sql a delta runs before or after it - statements, or a script file in deltas/scripts
type Hook struct {
	Sql    []string
	Script string
}

// PrePostHook is a delta's pre and post hooks
type PrePostHook struct {
	Pre  Hook
	Post Hook
}

// FkDelta adds or drops a foreign key defined in the delta itself
type FkDelta struct {
	Object     string
	Action     string
	Tablename  string
	Definition ForeignKey `json:"def"`
	PrePostHook
}

// SimpleDelta adds, drops, alters or renames an object - an add reads the object's definition from the state files
type SimpleDelta struct {
	Object    string
	Action    string
	Name      string
	Reference string
	NewName   string
	// On is what a comment delta comments on: table, column, view, enum, sequence or matview
	On string
	// Value is the value an enum alter delta adds
	Value string
	// Concurrently refreshes a materialized view without locking out reads
	Concurrently bool
	PrePostHook
}

// ColumnDelta adds, drops or alters a column - from the definition in the delta, or (for an alter) the table's state file
type ColumnDelta struct {
	Object     string
	Action     string
	Name       string
	Definition Column `json:"def"`
	Reference  string
	Mods       ColumnAlterAttributes `json:"mods"`
	PrePostHook
}

// SqlDelta runs sql - statements, or a script file in deltas/scripts - tolerating the error codes in bypass
type SqlDelta struct {
	Object      string
	Description string
	Script      string
	Sql         []string
	Bypass      []string
}

// ColumnAlterAttributes says which of a column's attributes an alter column delta changes
type ColumnAlterAttributes struct {
	Datatype bool
	Nullable bool
	Default  bool
}

// ChangeSet is a changeset file (deltas/v<major>_<minor>_<patch>.json) - its version comes from the file name
type ChangeSet struct {
	Version      string
	Description  string
	Deltas       []interface{}
	ParsedDeltas []Delta
	Filename     string
	SemVer       util.SemanticVersion
	// Transaction is false when the changeset contains statements that cannot run in a transaction (e.g. create index
	// concurrently). It defaults to true when not set
	Transaction *bool
}

// InTransaction reports whether the changeset should be applied in a single transaction
func (cs ChangeSet) InTransaction() bool {
	return cs.Transaction == nil || *cs.Transaction
}

// UniqueIndex reports whether the materialized view has a unique index on columns, with no where clause - which a
// refresh concurrently needs. An index defined by its ddl is not checked, so it counts
func (m Matview) UniqueIndex() bool {
	for _, idx := range m.Indexes {
		if idx.Ddl != "" || idx.Unique {
			return true
		}
	}
	return false
}

// MissingDefinitionError reports a delta whose definition is not in the state files, so it generates no DDL - the
// apply skips such a delta rather than failing
type MissingDefinitionError struct {
	msg string
}

// Column returns the table's column with the name
func (t Table) Column(name string) (Column, bool) {
	for _, col := range t.Columns {
		if col.Name == name {
			return col, true
		}
	}
	return Column{}, false
}

// Hooks returns the pre/post hooks - promoted to every delta type that embeds PrePostHook
func (hook PrePostHook) Hooks() PrePostHook {
	return hook
}

// HasPreHook reports whether the delta runs sql before it
func (hook PrePostHook) HasPreHook() bool {
	if len(hook.Pre.Script) > 0 || len(hook.Pre.Sql) > 0 {
		return true
	}
	return false
}

// HasPostHook reports whether the delta runs sql after it
func (hook PrePostHook) HasPostHook() bool {
	if len(hook.Post.Script) > 0 || len(hook.Post.Sql) > 0 {
		return true
	}
	return false
}
