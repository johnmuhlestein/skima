package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The file formats of the state kinds. Field order is the order properties are written in - the order the schemas and
// README document them. Optional properties are omitted when unset, except nullable, which is always written so a
// column's nullability is explicit.

type columnFile struct {
	Name       string `json:"name"`
	Datatype   string `json:"datatype"`
	Nullable   bool   `json:"nullable"`
	Default    string `json:"default,omitempty"`
	Dfltfunc   bool   `json:"dfltfunc,omitempty"`
	Primarykey bool   `json:"primarykey,omitempty"`
	Generate   string `json:"generate,omitempty"`
	Comment    string `json:"comment,omitempty"`
}

type referenceFile struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
}

type foreignKeyFile struct {
	Name       string        `json:"name"`
	Columns    []string      `json:"columns"`
	Reference  referenceFile `json:"reference"`
	DeleteRule string        `json:"deleterule,omitempty"`
	UpdateRule string        `json:"updaterule,omitempty"`
}

type indexFile struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns,omitempty"`
	Unique  bool     `json:"unique,omitempty"`
	Ddl     string   `json:"ddl,omitempty"`
}

type triggerFile struct {
	Name string `json:"name"`
	Ddl  string `json:"ddl"`
}

type constraintFile struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Columns   []string `json:"columns,omitempty"`
	Condition string   `json:"condition,omitempty"`
}

type tableFile struct {
	Schema      string           `json:"$schema,omitempty"`
	Name        string           `json:"name"`
	Columns     []columnFile     `json:"columns"`
	PrimaryKey  []string         `json:"primary-key,omitempty"`
	ForeignKeys []foreignKeyFile `json:"foreign-keys,omitempty"`
	Indexes     []indexFile      `json:"indexes,omitempty"`
	Triggers    []triggerFile    `json:"triggers,omitempty"`
	Constraints []constraintFile `json:"constraints,omitempty"`
	Comment     string           `json:"comment,omitempty"`
}

type viewFile struct {
	Schema  string   `json:"$schema,omitempty"`
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Sql     string   `json:"sql"`
	Comment string   `json:"comment,omitempty"`
}

type enumFile struct {
	Schema  string   `json:"$schema,omitempty"`
	Name    string   `json:"name"`
	Values  []string `json:"values"`
	Comment string   `json:"comment,omitempty"`
}

type sequenceFile struct {
	Schema    string `json:"$schema,omitempty"`
	Name      string `json:"name"`
	Datatype  string `json:"datatype,omitempty"`
	Start     *int64 `json:"start,omitempty"`
	Increment *int64 `json:"increment,omitempty"`
	Minvalue  *int64 `json:"minvalue,omitempty"`
	Maxvalue  *int64 `json:"maxvalue,omitempty"`
	Cache     *int64 `json:"cache,omitempty"`
	Cycle     bool   `json:"cycle,omitempty"`
	Ownedby   string `json:"ownedby,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

type matviewFile struct {
	Schema  string      `json:"$schema,omitempty"`
	Name    string      `json:"name"`
	Columns []string    `json:"columns"`
	Sql     string      `json:"sql"`
	Data    *bool       `json:"data,omitempty"`
	Indexes []indexFile `json:"indexes,omitempty"`
	Comment string      `json:"comment,omitempty"`
}

func indexFiles(indexes []Index) []indexFile {
	var files []indexFile
	for _, idx := range indexes {
		files = append(files, indexFile(idx))
	}
	return files
}

// FormatTable returns a table's canonical file content. schema is the file's "$schema" - empty to leave it out
func FormatTable(t Table, schema string) ([]byte, error) {
	file := tableFile{Schema: schema, Name: t.Name, PrimaryKey: t.PrimaryKey, Indexes: indexFiles(t.Indexes), Comment: t.Comment}
	for _, c := range t.Columns {
		file.Columns = append(file.Columns, columnFile(c))
	}
	for _, fk := range t.ForeignKeys {
		file.ForeignKeys = append(file.ForeignKeys, foreignKeyFile{Name: fk.Name, Columns: fk.Columns,
			Reference:  referenceFile{Table: fk.Reference.Table, Columns: fk.Reference.Columns},
			DeleteRule: fk.DeleteRule, UpdateRule: fk.UpdateRule})
	}
	for _, trg := range t.Triggers {
		file.Triggers = append(file.Triggers, triggerFile(trg))
	}
	for _, cons := range t.Constraints {
		file.Constraints = append(file.Constraints, constraintFile(cons))
	}
	return encodeFile(file)
}

// FormatView returns a view's canonical file content. schema is the file's "$schema" - empty to leave it out
func FormatView(v View, schema string) ([]byte, error) {
	return encodeFile(viewFile{Schema: schema, Name: v.Name, Columns: v.Columns, Sql: v.Sql, Comment: v.Comment})
}

// FormatEnum returns an enum's canonical file content. schema is the file's "$schema" - empty to leave it out
func FormatEnum(e Enum, schema string) ([]byte, error) {
	return encodeFile(enumFile{Schema: schema, Name: e.Name, Values: e.Values, Comment: e.Comment})
}

// FormatSequence returns a sequence's canonical file content. schema is the file's "$schema" - empty to leave it out
func FormatSequence(s Sequence, schema string) ([]byte, error) {
	return encodeFile(sequenceFile{Schema: schema, Name: s.Name, Datatype: s.Datatype, Start: s.Start, Increment: s.Increment,
		Minvalue: s.Minvalue, Maxvalue: s.Maxvalue, Cache: s.Cache, Cycle: s.Cycle, Ownedby: s.Ownedby, Comment: s.Comment})
}

// FormatMatview returns a materialized view's canonical file content. schema is the file's "$schema" - empty to leave
// it out
func FormatMatview(mv Matview, schema string) ([]byte, error) {
	return encodeFile(matviewFile{Schema: schema, Name: mv.Name, Columns: mv.Columns, Sql: mv.Sql, Data: mv.Data,
		Indexes: indexFiles(mv.Indexes), Comment: mv.Comment})
}

// encodeFile writes the canonical JSON: 2-space indent and a trailing newline. HTML escaping is off so SQL in view
// queries, check conditions and triggers keeps its < > & as written
func encodeFile(file any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(file); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// InvalidFileError is returned when a file can't be formatted because it doesn't validate - parsing it could silently
// drop content (e.g. a misspelled property), so it must be fixed first
type InvalidFileError struct {
	Validation FileValidation
}

func (e *InvalidFileError) Error() string {
	return fmt.Sprintf("%s is invalid - fix it before formatting (see skima manifest validate)", e.Validation.File)
}

// FormatStateFile returns the canonical content of a state file, and whether it differs from the file's current
// content. The file must validate against its schema; its "$schema" property is kept
func FormatStateFile(path string) ([]byte, bool, error) {
	kind, ok := KindOf(path)
	if !ok || kind == KindChangeset {
		return nil, false, fmt.Errorf("%s is not a state file - only <name>-table.json, -view.json, -enum.json, -sequence.json and -matview.json files are formatted", path)
	}
	if validation := ValidateFile(path, kind); !validation.Valid {
		return nil, false, &InvalidFileError{Validation: validation}
	}
	raw, err := reader.readFile(path)
	if err != nil {
		return nil, false, err
	}
	var header struct {
		Schema string `json:"$schema"`
	}
	if err = json.Unmarshal(raw, &header); err != nil {
		return nil, false, err
	}
	var formatted []byte
	switch kind {
	case KindTable:
		var t Table
		if t, err = parseTable(raw); err == nil {
			formatted, err = FormatTable(t, header.Schema)
		}
	case KindView:
		var v View
		if v, err = parseView(raw); err == nil {
			formatted, err = FormatView(v, header.Schema)
		}
	case KindEnum:
		var e Enum
		if e, err = ParseEnumByPath(path); err == nil {
			formatted, err = FormatEnum(e, header.Schema)
		}
	case KindSequence:
		var s Sequence
		if s, err = ParseSequenceByPath(path); err == nil {
			formatted, err = FormatSequence(s, header.Schema)
		}
	case KindMatview:
		var mv Matview
		if mv, err = ParseMatviewByPath(path); err == nil {
			formatted, err = FormatMatview(mv, header.Schema)
		}
	}
	if err != nil {
		return nil, false, err
	}
	return formatted, !bytes.Equal(formatted, raw), nil
}
