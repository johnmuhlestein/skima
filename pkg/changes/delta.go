package changes

import (
	"bytes"
	"encoding/json"
	"skima/pkg/manifest"
)

// ColumnDef is an explicit column definition in a delta (def) - used to add a column differently from its state
// definition, e.g. as nullable before a backfill
type ColumnDef struct {
	Name       string `json:"name"`
	Datatype   string `json:"datatype"`
	Nullable   bool   `json:"nullable"`
	Default    string `json:"default,omitempty"`
	Dfltfunc   bool   `json:"dfltfunc,omitempty"`
	Primarykey bool   `json:"primarykey,omitempty"`
	Generate   string `json:"generate,omitempty"`
	Comment    string `json:"comment,omitempty"`
}

func columnDef(c manifest.Column) *ColumnDef {
	return &ColumnDef{Name: c.Name, Datatype: c.Datatype, Nullable: c.Nullable, Default: c.Default, Dfltfunc: c.Dfltfunc,
		Primarykey: c.Primarykey, Generate: c.Generate, Comment: c.Comment}
}

// Mods says which attribute of a column an alter delta changes - exactly one is set
type Mods struct {
	Datatype bool `json:"datatype,omitempty"`
	Nullable bool `json:"nullable,omitempty"`
	Default  bool `json:"default,omitempty"`
}

// Delta is one delta of a changeset, as it is written in the changeset file. Field order is the order properties are
// written in
type Delta struct {
	Object      string     `json:"object"`
	Action      string     `json:"action,omitempty"`
	On          string     `json:"on,omitempty"`
	Name        string     `json:"name,omitempty"`
	Reference   string     `json:"reference,omitempty"`
	NewName     string     `json:"newname,omitempty"`
	Def         *ColumnDef `json:"def,omitempty"`
	Mods        *Mods      `json:"mods,omitempty"`
	Description string     `json:"description,omitempty"`
	Sql         []string   `json:"sql,omitempty"`
	Script      string     `json:"script,omitempty"`

	// phase orders the deltas in the changeset - see the phase constants
	phase int
}

// The order deltas run in, so what depends on something goes before it is dropped and after it is added, and the
// destructive steps come last
const (
	phaseDropForeignKeys = 5  // first: a foreign key depends on the primary key or unique constraint it references
	phaseDropDependents  = 10 // constraint, index, trigger and primary key drops
	phaseRenameColumns   = 20
	phaseAddTables       = 30
	phaseAddColumns      = 40 // with their backfills
	phaseAlterColumns    = 50
	phaseAddDependents   = 60 // primary key, constraint, index and trigger adds
	phaseAddForeignKeys  = 65
	phaseComments        = 70
	phaseDropColumns     = 80
	phaseDropTables      = 90
)

// Changeset is a changeset file's content
type Changeset struct {
	Description string `json:"description,omitempty"`
	// Transaction is false to apply the changeset without a transaction - nil leaves it out (the default, true)
	Transaction *bool   `json:"transaction,omitempty"`
	Deltas      []Delta `json:"deltas"`
}

// Format returns the changeset file content: 2-space indent and a trailing newline, with SQL kept as written
func (cs Changeset) Format() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cs); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
