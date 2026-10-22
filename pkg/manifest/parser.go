package manifest

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"skima/pkg/util"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

const fkddlTmpl = "alter table %s %s constraint %s foreign key (%s) references %s (%s)"
const idxddlTmpl = "create %s index if not exists %s on %s (%s)"
const addColTmpl = "alter table %s add column if not exists %s"
const colRenameTmpl = "alter table %s rename column %s to %s"
const dropTableTmpl = "drop table %s"
const dropColTmpl = "alter table %s drop column if exists %s"
const dropFkTmpl = "alter table %s drop constraint %s"
const dropTriggerTmpl = "drop trigger if exists %s on %s"
const dropIndexTmpl = "drop index if exists %s cascade"

//go:embed skima_schema_history-table.json
var SkimaSchemaHistoryTable []byte

//go:embed skima_schema_statements-table.json
var SkimaSchemaStatementsTable []byte

type osReader interface {
	readFile(name string) ([]byte, error)
	readDir(name string) ([]os.DirEntry, error)
}

type osReadWrapper struct{}

func (orw osReadWrapper) readFile(name string) ([]byte, error) {
	b, e := os.ReadFile(name)
	return b, e
}

func (orw osReadWrapper) readDir(name string) ([]os.DirEntry, error) {
	b, e := os.ReadDir(name)
	return b, e
}

var reader osReader

func init() {
	reader = osReadWrapper{}
}

type Delta interface {
	GenerateDdl() string
	StatementType() string
	Describe() string
}

type DeltaHook interface {
	HasPreHook() bool
	HasPostHook() bool
}

type PrimaryKeyRef struct {
	Table   string
	Columns []string
}

type ForeignKey struct {
	Name       string
	Columns    []string
	Reference  PrimaryKeyRef
	DeleteRule string
	UpdateRule string
}

type Index struct {
	Name    string
	Columns []string
	Unique  bool
	Ddl     string
}

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

type Trigger struct {
	Name string
	Ddl  string
}

type Constraint struct {
	Name      string
	Type      string
	Columns   []string
	Condition string
}

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

type Hook struct {
	Sql    []string
	Script string
}

type PrePostHook struct {
	Pre  Hook
	Post Hook
}

type FkDelta struct {
	Object     string
	Action     string
	Tablename  string
	Definition ForeignKey `json:"def"`
	PrePostHook
}

type SimpleDelta struct {
	Object    string
	Action    string
	Name      string
	Reference string
	NewName   string
	// On is what a comment delta comments on: table, column, view, enum or sequence
	On string
	// Value is the value an enum alter delta adds
	Value string
	PrePostHook
}

type ColumnDelta struct {
	Object     string
	Action     string
	Name       string
	Definition Column `json:"def"`
	Reference  string
	Mods       ColumnAlterAttributes `json:"mods"`
	PrePostHook
}

type SqlDelta struct {
	Object      string
	Description string
	Script      string
	Sql         []string
	Bypass      []string
}

type ColumnAlterAttributes struct {
	Datatype bool
	Nullable bool
	Default  bool
}

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

func check(e error, msg string) {
	if e != nil {
		fmt.Fprintln(os.Stderr, msg)
		panic(e)
	}
}

// ParseChangeSet takes a file path to a ChangeSet file and
// parses out the ChangeSet information including parsing the
// Deltas that are part of the ChangeSet
// An error is returned if there are any issues parsing the file
func ParseChangeSet(path string) (ChangeSet, error) {
	activeChangeSet := ChangeSet{}
	namePart := filepath.Base(path)
	csBytes, err := reader.readFile(path)
	var data []byte
	if err == nil {
		err = json.Unmarshal(csBytes, &activeChangeSet)
		activeChangeSet.Version = util.ExtractVersion(namePart).String()
		activeChangeSet.SemVer = util.ExtractVersion(activeChangeSet.Version)
		activeChangeSet.ParsedDeltas = make([]Delta, len(activeChangeSet.Deltas))
		activeChangeSet.Filename = filepath.Base(path)
		for idx, delta := range activeChangeSet.Deltas {
			objType := delta.(map[string]interface{})["object"].(string)
			var simple bool
			// script deltas are always SqlDeltas - one that points at a script file has none of the other markers
			if objType != "script" && delta.(map[string]interface{})["def"] == nil && delta.(map[string]interface{})["mods"] == nil && delta.(map[string]interface{})["sql"] == nil {
				simple = true
			}
			if simple {
				data, err = json.Marshal(delta)
				if err == nil {
					chg := SimpleDelta{}
					err = json.Unmarshal(data, &chg)
					if err == nil {
						activeChangeSet.ParsedDeltas[idx] = chg
					}
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "Unable to marshal/unmarshal the delta json", err)
					return activeChangeSet, err
				}
			} else {
				// this is the ugly stuff - marshal and unmarshal
				data, err = json.Marshal(delta)
				if err == nil {
					switch objType {
					case "fk":
						fkd := FkDelta{}
						err = json.Unmarshal(data, &fkd)
						if err == nil {
							activeChangeSet.ParsedDeltas[idx] = fkd
						}
					case "column":
						cd := ColumnDelta{}
						err = json.Unmarshal(data, &cd)
						if err == nil {
							activeChangeSet.ParsedDeltas[idx] = cd
						}
					default:
						sd := SqlDelta{}
						err = json.Unmarshal(data, &sd)
						if err == nil {
							activeChangeSet.ParsedDeltas[idx] = sd
						}
					}
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "Unable to marshal/unmarshal the delta json", err)
					return activeChangeSet, err
				}
			}
		}
	}
	return activeChangeSet, err
}

// Version returns a SemanticVersion for the manifest based on
// the delta files in the manifest
func Version() util.SemanticVersion {
	deltaFilePaths, _ := ListFiles(filepath.Join(viper.GetString("workdir"), "deltas"), "json")
	if len(deltaFilePaths) == 0 {
		return util.ExtractVersion("1.0.0")
	}
	// the manifest version is the highest changeset version, whatever its value
	ver := util.ExtractVersion(filepath.Base(deltaFilePaths[0]))
	for _, path := range deltaFilePaths[1:] {
		if fileVer := util.ExtractVersion(filepath.Base(path)); fileVer.Compare(ver) > 0 {
			ver = fileVer
		}
	}
	return ver
}

func ParseTableByName(name string) (Table, error) {
	switch name {
	case "skima_schema_history":
		return parseTable(SkimaSchemaHistoryTable)
	case "skima_schema_statements":
		return parseTable(SkimaSchemaStatementsTable)
	default:
		path := filepath.Join(viper.GetString("workdir"), "state", fmt.Sprintf("%s-table.json", name))
		return ParseTableByPath(path)
	}
}

func ParseTableByPath(path string) (Table, error) {
	entityBytes, err := reader.readFile(path)
	if err == nil {
		return parseTable(entityBytes)
	}
	return Table{}, err
}

func ParseEnumByName(name string) (Enum, error) {
	path := filepath.Join(viper.GetString("workdir"), "state", fmt.Sprintf("%s-enum.json", name))
	return ParseEnumByPath(path)
}

func ParseEnumByPath(path string) (Enum, error) {
	entityBytes, err := reader.readFile(path)
	if err != nil {
		return Enum{}, err
	}
	enum := Enum{}
	err = json.Unmarshal(entityBytes, &enum)
	return enum, err
}

// GenerateDdl creates the enum type with its values
func (e Enum) GenerateDdl() string {
	values := make([]string, len(e.Values))
	for i, value := range e.Values {
		values[i] = QuoteLiteral(value)
	}
	return fmt.Sprintf("create type %s as enum (%s)", e.Name, strings.Join(values, ", "))
}

// AddValueDdl adds the value to the type in the position the enum gives it - after the value before it, or before the
// value after it when it is first. The value must be one of the enum's values
func (e Enum) AddValueDdl(value string) (string, bool) {
	position := slices.Index(e.Values, value)
	if position < 0 {
		return "", false
	}
	ddl := fmt.Sprintf("alter type %s add value if not exists %s", e.Name, QuoteLiteral(value))
	switch {
	case position > 0:
		ddl += " after " + QuoteLiteral(e.Values[position-1])
	case len(e.Values) > 1:
		ddl += " before " + QuoteLiteral(e.Values[1])
	}
	return ddl, true
}

func ParseSequenceByName(name string) (Sequence, error) {
	path := filepath.Join(viper.GetString("workdir"), "state", fmt.Sprintf("%s-sequence.json", name))
	return ParseSequenceByPath(path)
}

func ParseSequenceByPath(path string) (Sequence, error) {
	entityBytes, err := reader.readFile(path)
	if err != nil {
		return Sequence{}, err
	}
	seq := Sequence{}
	err = json.Unmarshal(entityBytes, &seq)
	return seq, err
}

// GenerateDdl creates the sequence with the settings it defines - its owner is set separately, once the owning table
// exists (OwnedByDdl)
func (s Sequence) GenerateDdl() string {
	var bld strings.Builder
	fmt.Fprintf(&bld, "create sequence %s", s.Name)
	if s.Datatype != "" {
		fmt.Fprintf(&bld, " as %s", s.Datatype)
	}
	for _, setting := range []struct {
		keyword string
		value   *int64
	}{{"increment by", s.Increment}, {"minvalue", s.Minvalue}, {"maxvalue", s.Maxvalue}, {"start with", s.Start}, {"cache", s.Cache}} {
		if setting.value != nil {
			fmt.Fprintf(&bld, " %s %d", setting.keyword, *setting.value)
		}
	}
	if s.Cycle {
		bld.WriteString(" cycle")
	}
	return bld.String()
}

// OwnedByDdl makes the sequence's ownedby column its owner
func (s Sequence) OwnedByDdl() string {
	return fmt.Sprintf("alter sequence %s owned by %s", s.Name, s.Ownedby)
}

// AlterDdl brings every setting and the owner of an existing sequence in line with the definition - a setting that is
// not defined goes back to postgres' default. The sequence is not restarted, so start is left as it is
func (s Sequence) AlterDdl() string {
	var bld strings.Builder
	datatype := s.Datatype
	if datatype == "" {
		datatype = "bigint"
	}
	fmt.Fprintf(&bld, "alter sequence %s as %s", s.Name, datatype)
	orDefault := func(value *int64, fallback int64) int64 {
		if value != nil {
			return *value
		}
		return fallback
	}
	fmt.Fprintf(&bld, " increment by %d", orDefault(s.Increment, 1))
	for _, limit := range []struct {
		keyword string
		value   *int64
	}{{"minvalue", s.Minvalue}, {"maxvalue", s.Maxvalue}} {
		if limit.value != nil {
			fmt.Fprintf(&bld, " %s %d", limit.keyword, *limit.value)
		} else {
			fmt.Fprintf(&bld, " no %s", limit.keyword)
		}
	}
	fmt.Fprintf(&bld, " cache %d", orDefault(s.Cache, 1))
	if s.Cycle {
		bld.WriteString(" cycle")
	} else {
		bld.WriteString(" no cycle")
	}
	if s.Ownedby != "" {
		fmt.Fprintf(&bld, " owned by %s", s.Ownedby)
	} else {
		bld.WriteString(" owned by none")
	}
	return bld.String()
}

func ParseViewByName(name string) (View, error) {
	path := filepath.Join(viper.GetString("workdir"), "state", fmt.Sprintf("%s-view.json", name))
	return ParseViewByPath(path)
}

func ParseViewByPath(path string) (View, error) {
	entityBytes, err := reader.readFile(path)
	if err == nil {
		return parseView(entityBytes)
	}
	return View{}, err
}

func parseTable(raw []byte) (Table, error) {
	table := Table{}
	err := json.Unmarshal(raw, &table)
	return table, err
}

func parseView(raw []byte) (View, error) {
	view := View{}
	err := json.Unmarshal(raw, &view)
	return view, err
}

// ListFiles returns a list of files from a given path with a provided suffix
func ListFiles(path string, sfx string) ([]string, error) {
	files := make([]string, 0)
	c, err := reader.readDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return files, nil // a missing directory (e.g. no deltas yet) has no files - other errors are returned
	}
	if err == nil {
		for _, entry := range c {
			if filepath.Ext(entry.Name()) == fmt.Sprintf(".%s", sfx) {
				files = append(files, filepath.Join(path, entry.Name()))
			}
		}
	}
	return files, err
}

func (v View) GenerateDdl() string {
	var bld strings.Builder
	bld.WriteString("create or replace view ")
	bld.WriteString(v.Name)
	fmt.Fprintf(&bld, " (%s) as %s", strings.Join(v.Columns, ","), v.Sql)
	return bld.String()
}

func (col Column) AlterStmt(tablename string, action string) string {
	switch action {
	case "add":
		var bld strings.Builder
		fmt.Fprintf(&bld, addColTmpl, tablename, col.Name)
		bld.WriteString(" ")
		bld.WriteString(col.Datatype)
		if !col.Nullable {
			bld.WriteString(" not null")
		}
		if col.Default != "" {
			bld.WriteString(col.defaultBuilder())
		}
		return bld.String()
	case "drop":
		return fmt.Sprintf(dropColTmpl, tablename, col.Name)
	}
	return ""
}

func (col Column) defaultBuilder() string {
	numericType := util.FromSlice([]string{"smallint", "integer", "bigint", "numeric", "decimal"})
	if col.Default != "" {
		if col.Dfltfunc || col.Datatype == "bool" || numericType.Has(col.Datatype) {
			return fmt.Sprintf(" default %s", col.Default)
		} else {
			return fmt.Sprintf(" default '%s'", col.Default)
		}
	}
	return ""
}

func (fk ForeignKey) AlterStmt(tableName string, action string) string {
	var bld strings.Builder
	fmt.Fprintf(&bld, fkddlTmpl, tableName, action, fk.Name, strings.Join(fk.Columns, ", "), fk.Reference.Table, strings.Join(fk.Reference.Columns, ", "))
	if len(fk.UpdateRule) > 0 {
		bld.WriteString(" on update ")
		bld.WriteString(fk.UpdateRule)
	}
	if len(fk.DeleteRule) > 1 {
		bld.WriteString(" on delete ")
		bld.WriteString(fk.DeleteRule)
	}
	return bld.String()
}

func (idx Index) GenerateDdl(tableName string) string {
	if len(idx.Ddl) == 0 {
		var unique string
		if idx.Unique {
			unique = "unique"
		}
		idx.Ddl = fmt.Sprintf(idxddlTmpl, unique, idx.Name, tableName, strings.Join(idx.Columns, ", "))
	}
	return idx.Ddl
}

func (cons Constraint) GenerateDdl(tableName string) string {
	var bld strings.Builder
	fmt.Fprintf(&bld, "alter table %s add constraint %s ", tableName, cons.Name)
	switch cons.Type {
	case "unique":
		fmt.Fprintf(&bld, "unique (%s)", strings.Join(cons.Columns, ","))
	case "check":
		fmt.Fprintf(&bld, "check (%s)", cons.Condition)
	}
	return bld.String()
}

func (fk FkDelta) GenerateDdl() string {
	return fk.Definition.AlterStmt(fk.Tablename, fk.Action)
}

func (fk FkDelta) StatementType() string {
	return fmt.Sprintf("delta %s %s", fk.Action, fk.Object)
}

func (fk FkDelta) Describe() string {
	return fmt.Sprintf("%s %s.%s", fk.Action, fk.Tablename, fk.Definition.Name)
}

func (col ColumnDelta) GenerateDdl() string {
	ddl, err := col.Ddl()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ALTER COLUMN datatype failure due to not being able to parse table reference: %s", err)
		return ""
	}
	return ddl
}

// Ddl generates the delta's statement, returning an error if the reference table's state file cannot be read
func (col ColumnDelta) Ddl() (string, error) {
	if col.Definition.Name != "" {
		return col.Definition.AlterStmt(col.Reference, col.Action), nil
	}
	refTable, err := ParseTableByName(col.Reference)
	var refColumn Column
	var defaultString string
	if err != nil {
		return "", err
	}
	for _, rc := range refTable.Columns {
		if rc.Name == col.Name {
			refColumn = rc
			defaultString = refColumn.defaultBuilder()
		}
	}
	builder := strings.Builder{}
	fmt.Fprintf(&builder, "alter table %s alter column %s ", col.Reference, col.Name)
	if col.Mods.Nullable {
		switch refColumn.Nullable {
		case true:
			builder.WriteString("drop NOT NULL")
		case false:
			builder.WriteString("set NOT NULL")
		}
	} else if col.Mods.Datatype {
		fmt.Fprintf(&builder, "set data type %s", refColumn.Datatype)
	} else if col.Mods.Default {
		if len(defaultString) > 0 {
			fmt.Fprintf(&builder, "set %s", strings.Trim(defaultString, " "))
		} else {
			builder.WriteString("drop default")
		}
	}
	return builder.String(), nil
}

func (col ColumnDelta) StatementType() string {
	return fmt.Sprintf("delta %s %s", col.Action, col.Object)
}

func (col ColumnDelta) Describe() string {
	return fmt.Sprintf("%s %s.%s", col.Action, col.Reference, col.Name)
}

func (sd SimpleDelta) Table() Table {
	var tbl Table
	var err error
	if sd.Object == "table" && sd.Action == "add" {
		tbl, err = ParseTableByName(sd.Name)
		check(err, fmt.Sprintf("Unable to parse the table %s", sd.Name))
	}
	return tbl
}

func (sd SimpleDelta) GenerateDdl() string {
	ddl, err := sd.Ddl()
	var missing *MissingDefinitionError
	if errors.As(err, &missing) {
		fmt.Fprintln(os.Stderr, missing)
		return ""
	}
	if err != nil {
		check(err, err.Error())
	}
	return ddl
}

// MissingDefinitionError reports a delta whose definition is not in the state files, so it generates no DDL - the
// apply skips such a delta rather than failing
type MissingDefinitionError struct {
	msg string
}

func (e *MissingDefinitionError) Error() string {
	return e.msg
}

func missingDefinition(format string, args ...any) error {
	return &MissingDefinitionError{msg: fmt.Sprintf(format, args...)}
}

// Ddl generates the delta's statement. It returns a *MissingDefinitionError when the definition it reads from the state
// files is not there, and other errors when a state or sql file cannot be read. An object/action skima does not
// support generates an empty statement and no error
func (sd SimpleDelta) Ddl() (string, error) {
	switch sd.Object {
	case "table":
		if sd.Action == "add" {
			t, err := ParseTableByName(sd.Name)
			if err != nil {
				return "", fmt.Errorf("Unable to parse the table %s: %w", sd.Name, err)
			}
			return t.GenerateDdl(), nil
		} else if sd.Action == "drop" {
			return fmt.Sprintf(dropTableTmpl, sd.Name), nil
		}
	case "view":
		switch sd.Action {
		case "add":
			v, err := ParseViewByName(sd.Name)
			if err != nil {
				return "", fmt.Errorf("Unable to parse the view %s: %w", sd.Name, err)
			}
			return v.GenerateDdl(), nil
		case "drop":
			return fmt.Sprintf("drop view if exists %s", sd.Name), nil
		}
	case "column":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropColTmpl, sd.Reference, sd.Name), nil
		case "rename":
			return fmt.Sprintf(colRenameTmpl, sd.Reference, sd.Name, sd.NewName), nil
		case "add":
			if len(sd.Reference) == 0 {
				return "", missingDefinition("cannot add column %s as a simple delta without a reference value", sd.Name)
			}
			t, err := sd.referenceTable()
			if err != nil {
				return "", err
			}
			for _, col := range t.Columns {
				if col.Name == sd.Name {
					return col.AlterStmt(t.Name, sd.Action), nil
				}
			}
			return "", missingDefinition("Table %s did not have a column definition matching %s", sd.Reference, sd.Name)
		}
	case "fk":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropFkTmpl, sd.Reference, sd.Name), nil
		case "add":
			if len(sd.Reference) > 0 {
				t, err := sd.referenceTable()
				if err != nil {
					return "", err
				}
				for _, fk := range t.ForeignKeys {
					if fk.Name == sd.Name {
						return fk.AlterStmt(t.Name, sd.Action), nil
					}
				}
				return "", missingDefinition("Table %s did not have a foreign key definition matching %s", sd.Reference, sd.Name)
			}
		}
	case "pk":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropFkTmpl, sd.Reference, sd.Name), nil
		case "add":
			if len(sd.Reference) > 0 {
				t, err := sd.referenceTable()
				if err != nil {
					return "", err
				}
				if len(t.PrimaryKey) == 0 {
					fmt.Fprintf(os.Stderr, "Table %s did not have a primary key definition\n", sd.Reference)
				}
				return fmt.Sprintf("alter table %s add constraint %s primary key (%s)", sd.Reference, sd.Name, strings.Join(t.PrimaryKey, ", ")), nil
			}
		}
	case "trigger":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropTriggerTmpl, sd.Name, sd.Reference), nil
		case "add":
			if len(sd.Reference) > 0 {
				t, err := sd.referenceTable()
				if err != nil {
					return "", err
				}
				for _, trg := range t.Triggers {
					if trg.Name == sd.Name {
						return trg.Ddl, nil
					}
				}
				return "", missingDefinition("Table %s did not have a trigger definition matching %s", sd.Reference, sd.Name)
			}
		}
	case "function":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf("drop function if exists %s", sd.Name), nil
		case "add":
			path, err := sd.FunctionFile()
			if err != nil {
				return "", err
			}
			dat, err := reader.readFile(path)
			if err != nil {
				return "", fmt.Errorf("Unable to open the sql file for %s - expected %s: %w", sd.Name, strings.Join(FunctionFilePaths("", sd.Name), " or "), err)
			}
			return string(dat), nil
		}
	case "index":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropIndexTmpl, sd.Name), nil
		case "add":
			if len(sd.Reference) > 0 {
				t, err := sd.referenceTable()
				if err != nil {
					return "", err
				}
				for _, idx := range t.Indexes {
					if idx.Name == sd.Name {
						return idx.GenerateDdl(sd.Reference), nil
					}
				}
				return "", missingDefinition("Table %s did not have a index definition matching %s", sd.Reference, sd.Name)
			}
		}
	case "enum":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf("drop type if exists %s", sd.Name), nil
		case "add", "alter":
			e, err := ParseEnumByName(sd.Name)
			if err != nil {
				return "", fmt.Errorf("Unable to parse the enum %s: %w", sd.Name, err)
			}
			if sd.Action == "add" {
				return e.GenerateDdl(), nil
			}
			if ddl, ok := e.AddValueDdl(sd.Value); ok {
				return ddl, nil
			}
			return "", missingDefinition("Enum %s did not have a value matching %s", sd.Name, sd.Value)
		}
	case "sequence":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf("drop sequence if exists %s", sd.Name), nil
		case "add", "alter":
			seq, err := ParseSequenceByName(sd.Name)
			if err != nil {
				return "", fmt.Errorf("Unable to parse the sequence %s: %w", sd.Name, err)
			}
			if sd.Action == "add" {
				return seq.GenerateDdl(), nil
			}
			return seq.AlterDdl(), nil
		}
	case "comment":
		if sd.Action == "drop" {
			return CommentDdl(sd.On, sd.commentTarget(), ""), nil
		}
		comment, err := sd.stateComment()
		if err != nil {
			return "", err
		}
		return CommentDdl(sd.On, sd.commentTarget(), comment), nil
	case "constraint":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf("alter table %s drop constraint if exists %s", sd.Reference, sd.Name), nil
		case "add":
			if len(sd.Reference) > 0 {
				t, err := sd.referenceTable()
				if err != nil {
					return "", err
				}
				for _, cons := range t.Constraints {
					if cons.Name == sd.Name {
						return cons.GenerateDdl(sd.Reference), nil
					}
				}
				return "", missingDefinition("Table %s did not have an constraint definition matching %s", sd.Reference, sd.Name)
			}
		}
	}
	return "", nil
}

// commentTarget is what a comment delta comments on - table.column for a column, otherwise the name
func (sd SimpleDelta) commentTarget() string {
	if sd.On == "column" {
		return sd.Reference + "." + sd.Name
	}
	return sd.Name
}

// stateComment reads the comment a comment delta sets from the state files
func (sd SimpleDelta) stateComment() (string, error) {
	var comment string
	switch sd.On {
	case "table":
		t, err := ParseTableByName(sd.Name)
		if err != nil {
			return "", fmt.Errorf("Unable to parse the table %s: %w", sd.Name, err)
		}
		comment = t.Comment
	case "column":
		t, err := sd.referenceTable()
		if err != nil {
			return "", err
		}
		col, ok := t.Column(sd.Name)
		if !ok {
			return "", missingDefinition("Table %s did not have a column definition matching %s", sd.Reference, sd.Name)
		}
		comment = col.Comment
	case "view":
		v, err := ParseViewByName(sd.Name)
		if err != nil {
			return "", fmt.Errorf("Unable to parse the view %s: %w", sd.Name, err)
		}
		comment = v.Comment
	case "enum":
		e, err := ParseEnumByName(sd.Name)
		if err != nil {
			return "", fmt.Errorf("Unable to parse the enum %s: %w", sd.Name, err)
		}
		comment = e.Comment
	case "sequence":
		seq, err := ParseSequenceByName(sd.Name)
		if err != nil {
			return "", fmt.Errorf("Unable to parse the sequence %s: %w", sd.Name, err)
		}
		comment = seq.Comment
	default:
		return "", missingDefinition("cannot comment on %q - use table, column, view, enum or sequence", sd.On)
	}
	if comment == "" {
		return "", missingDefinition("%s %s has no comment in its state file", sd.On, sd.commentTarget())
	}
	return comment, nil
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

// CommentDdl sets the comment on a table, column (target is table.column), view, enum or sequence - an empty comment
// removes it.
// The comment is quoted as a string literal
func CommentDdl(on string, target string, comment string) string {
	literal := "null"
	if comment != "" {
		literal = QuoteLiteral(comment)
	}
	if on == "enum" {
		on = "type" // an enum is a type to postgres
	}
	return fmt.Sprintf("comment on %s %s is %s", on, target, literal)
}

// QuoteLiteral quotes text as a postgres string literal. With standard_conforming_strings on (the default since
// postgres 9.1) backslashes in a literal are ordinary characters, so only single quotes are doubled
func QuoteLiteral(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}

func (sd SimpleDelta) referenceTable() (Table, error) {
	t, err := ParseTableByName(sd.Reference)
	if err != nil {
		return t, fmt.Errorf("Unable to parse the reference table %s: %w", sd.Reference, err)
	}
	return t, nil
}

// FunctionFile returns the sql file a function add delta runs - the first of FunctionFilePaths that exists. When none
// can be read it returns the error reading the last of them
func (sd SimpleDelta) FunctionFile() (string, error) {
	var err error
	for _, path := range FunctionFilePaths(viper.GetString("workdir"), sd.Name) {
		if _, err = reader.readFile(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("Unable to open the sql file for %s - expected %s: %w", sd.Name, strings.Join(FunctionFilePaths("", sd.Name), " or "), err)
}

// FunctionFilePaths returns where a function delta's sql file can be, in lookup order: state/sql/pre (where the state
// build also runs it) and then state/sql, where function files were originally read from
func FunctionFilePaths(workdir string, name string) []string {
	return []string{
		filepath.Join(workdir, "state", "sql", "pre", name+".sql"),
		filepath.Join(workdir, "state", "sql", name+".sql"),
	}
}

func (sd SimpleDelta) StatementType() string {
	return fmt.Sprintf("delta %s %s", sd.Action, sd.Object)
}

func (sd SimpleDelta) Describe() string {
	var bld strings.Builder
	bld.WriteString(sd.Action)
	bld.WriteString(" ")
	if sd.Object == "comment" {
		fmt.Fprintf(&bld, "comment on %s ", sd.On)
	}
	if sd.Object == "enum" && sd.Action == "alter" {
		fmt.Fprintf(&bld, "%s (add value %s)", sd.Name, QuoteLiteral(sd.Value))
		return bld.String()
	}
	if len(sd.Reference) > 0 {
		bld.WriteString(sd.Reference)
		bld.WriteString(".")
	}
	bld.WriteString(sd.Name)
	return bld.String()
}

func (sd SqlDelta) Describe() string {
	return sd.Description
}

func (sd SqlDelta) StatementType() string {
	return "Standalone SQL delta"
}

func (sd SqlDelta) GenerateDdl() string {
	if len(sd.Script) > 0 {
		dat, err := reader.readFile(filepath.Join(viper.GetString("workdir"), "deltas", "scripts", sd.Script))
		if err == nil {
			return string(dat)
		} else {
			fmt.Fprintln(os.Stderr, "Unable to read the delta script file", sd.Script)
			return ""
		}
	}
	return sd.Sql[0]
}

// ScriptPath returns where a script delta or hook's sql file is - in the deltas/scripts directory
func ScriptPath(workdir string, script string) string {
	return filepath.Join(workdir, "deltas", "scripts", script)
}

// readScript reads a script file, returning an error if it is missing or empty - applying it would fail
func readScript(script string) (string, error) {
	dat, err := reader.readFile(ScriptPath(viper.GetString("workdir"), script))
	if err != nil {
		return "", fmt.Errorf("unable to read the script %s: %w", script, err)
	}
	if len(dat) == 0 {
		return "", fmt.Errorf("the script %s is empty", script)
	}
	return string(dat), nil
}

// Statements returns every statement the delta runs - its sql, or the contents of its script file as one statement
func (sd SqlDelta) Statements() ([]string, error) {
	if len(sd.Script) > 0 {
		script, err := readScript(sd.Script)
		if err != nil {
			return nil, err
		}
		return []string{script}, nil
	}
	return sd.Sql, nil
}

// Statements returns the hook's sql, or the contents of its script file as one statement when it has no sql
func (hook Hook) Statements() ([]string, error) {
	if len(hook.Sql) > 0 {
		return hook.Sql, nil
	}
	if len(hook.Script) > 0 {
		script, err := readScript(hook.Script)
		if err != nil {
			return nil, err
		}
		return []string{script}, nil
	}
	return nil, nil
}

// Hooks returns the pre/post hooks - promoted to every delta type that embeds PrePostHook
func (hook PrePostHook) Hooks() PrePostHook {
	return hook
}

func (hook PrePostHook) HasPreHook() bool {
	if len(hook.Pre.Script) > 0 || len(hook.Pre.Sql) > 0 {
		return true
	}
	return false
}

func (hook PrePostHook) HasPostHook() bool {
	if len(hook.Post.Script) > 0 || len(hook.Post.Sql) > 0 {
		return true
	}
	return false
}

func (t Table) GenerateDdl() string {
	var bld strings.Builder
	var ignorePk bool = false
	fmt.Fprintf(&bld, "create table if not exists %s (", t.Name)
	for i, col := range t.Columns {
		if i > 0 {
			bld.WriteString(",")
		}
		bld.WriteString(fmt.Sprintf("%s %s", col.Name, col.Datatype))
		if col.Primarykey && col.Generate != "" {
			fmt.Fprintf(&bld, " primary key generated %s as identity", col.Generate)
			ignorePk = true
		}
		if !col.Nullable && !col.Primarykey {
			bld.WriteString(" not null")
		}
		if col.Default != "" {
			bld.WriteString(col.defaultBuilder())
		}
	}
	if len(t.PrimaryKey) > 0 && !ignorePk {
		fmt.Fprintf(&bld, " , constraint %s_pk primary key (%s)", t.Name, strings.Join(t.PrimaryKey, ", "))
	}
	bld.WriteString(") ")

	return bld.String()
}

func (t Table) GenerateFkDdl() []string {
	fks := make([]string, 0)
	for _, fk := range t.ForeignKeys {
		fks = append(fks, fk.AlterStmt(t.Name, "add"))
	}
	return fks
}
