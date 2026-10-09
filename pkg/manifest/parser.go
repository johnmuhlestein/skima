package manifest

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"skima/pkg/util"
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
}

type View struct {
	Name    string
	Columns []string
	Sql     string
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
		fmt.Println("No delta files found, going with version 1.0.0")
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
	switch err.(type) {
	case *fs.PathError:
		fmt.Printf("Path %s does not exist - will return an empty list of files\n", path)
		return files, nil
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
	if col.Definition.Name != "" {
		return col.Definition.AlterStmt(col.Reference, col.Action)
	}
	refTable, err := ParseTableByName(col.Reference)
	var refColumn Column
	var defaultString string
	if err == nil {
		for _, rc := range refTable.Columns {
			if rc.Name == col.Name {
				refColumn = rc
				defaultString = refColumn.defaultBuilder()
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "ALTER COLUMN datatype failure due to not being able to parse table reference: %s", err)
		return ""
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
	return builder.String()
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
	switch sd.Object {
	case "table":
		if sd.Action == "add" {
			t, err := ParseTableByName(sd.Name)
			check(err, fmt.Sprintf("Unable to parse the table %s", sd.Name))
			return t.GenerateDdl()
		} else if sd.Action == "drop" {
			return fmt.Sprintf(dropTableTmpl, sd.Name)
		}
	case "view":
		switch sd.Action {
		case "add":
			v, err := ParseViewByName(sd.Name)
			check(err, fmt.Sprintf("Unable to parse the view %s", sd.Name))
			return v.GenerateDdl()
		case "drop":
			return fmt.Sprintf("drop view if exists %s", sd.Name)
		}
	case "column":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropColTmpl, sd.Reference, sd.Name)
		case "rename":
			return fmt.Sprintf(colRenameTmpl, sd.Reference, sd.Name, sd.NewName)
		case "add":
			if len(sd.Reference) > 0 {
				t, err := ParseTableByName(sd.Reference)
				check(err, fmt.Sprintf("Unable to parse the reference table %s", sd.Reference))
				for _, col := range t.Columns {
					if col.Name == sd.Name {
						return col.AlterStmt(t.Name, sd.Action)
					}
				}
				fmt.Fprintf(os.Stderr, "Table %s did not have a column definition matching %s\n", sd.Reference, sd.Name)
			} else {
				fmt.Fprintf(os.Stderr, "cannot add column %s as a simple delta without a reference value\n", sd.Name)
			}
		}
	case "fk":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropFkTmpl, sd.Reference, sd.Name)
		case "add":
			if len(sd.Reference) > 0 {
				t, err := ParseTableByName(sd.Reference)
				check(err, fmt.Sprintf("Unable to parse the reference table %s", sd.Reference))
				for _, fk := range t.ForeignKeys {
					if fk.Name == sd.Name {
						return fk.AlterStmt(t.Name, sd.Action)
					}
				}
				fmt.Fprintf(os.Stderr, "Table %s did not have a foreign key definition matching %s\n", sd.Reference, sd.Name)
			}
		}
	case "pk":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropFkTmpl, sd.Reference, sd.Name)
		case "add":
			if len(sd.Reference) > 0 {
				t, err := ParseTableByName(sd.Reference)
				check(err, fmt.Sprintf("Unable to parse the reference table %s", sd.Reference))
				if (t.PrimaryKey == nil) || (len(t.PrimaryKey) == 0) {
					fmt.Fprintf(os.Stderr, "Table %s did not have a primary key definition\n", sd.Reference)
				}
				return fmt.Sprintf("alter table %s add constraint %s primary key (%s)", sd.Reference, sd.Name, strings.Join(t.PrimaryKey, ", "))
			}
		}
	case "trigger":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropTriggerTmpl, sd.Name, sd.Reference)
		case "add":
			if len(sd.Reference) > 0 {
				t, err := ParseTableByName(sd.Reference)
				check(err, fmt.Sprintf("Unable to parse the reference table %s", sd.Reference))
				for _, trg := range t.Triggers {
					if trg.Name == sd.Name {
						return trg.Ddl
					}
				}
				fmt.Fprintf(os.Stderr, "Table %s did not have a trigger definition matching %s\n", sd.Reference, sd.Name)
			}
		}
	case "function":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf("drop function if exists %s", sd.Name)
		case "add":
			var dat []byte
			var err error
			for _, path := range FunctionFilePaths(viper.GetString("workdir"), sd.Name) {
				if dat, err = reader.readFile(path); err == nil {
					break
				}
			}
			check(err, fmt.Sprintf("Unable to open the sql file for %s - expected %s", sd.Name, strings.Join(FunctionFilePaths("", sd.Name), " or ")))
			return string(dat)
		}
	case "index":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropIndexTmpl, sd.Name)
		case "add":
			if len(sd.Reference) > 0 {
				t, err := ParseTableByName(sd.Reference)
				check(err, fmt.Sprintf("Unable to parse the reference table %s", sd.Reference))
				for _, idx := range t.Indexes {
					if idx.Name == sd.Name {
						return idx.GenerateDdl(sd.Reference)
					}
				}
				fmt.Fprintf(os.Stderr, "Table %s did not have a index definition matching %s\n", sd.Reference, sd.Name)
			}
		}
	case "constraint":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf("alter table %s drop constraint if exists %s", sd.Reference, sd.Name)
		case "add":
			if len(sd.Reference) > 0 {
				t, err := ParseTableByName(sd.Reference)
				check(err, fmt.Sprintf("Unable to parse the reference table %s", sd.Reference))
				for _, cons := range t.Constraints {
					if cons.Name == sd.Name {
						return cons.GenerateDdl(sd.Reference)
					}
				}
				fmt.Fprintf(os.Stderr, "Table %s did not have an constraint definition matching %s\n", sd.Reference, sd.Name)
			}
		}
	}
	return ""
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
