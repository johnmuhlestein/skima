package manifest

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"github.com/johnmuhlestein/skima/internal/util"
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

// GenerateDdl creates the enum type with its values
func (e Enum) GenerateDdl() string {
	values := make([]string, len(e.Values))
	for i, value := range e.Values {
		values[i] = QuoteLiteral(value)
	}
	return fmt.Sprintf("create type %s as enum (%s)", QuoteIdent(e.Name), strings.Join(values, ", "))
}

// AddValueDdl adds the value to the type in the position the enum gives it - after the value before it, or before the
// value after it when it is first. The value must be one of the enum's values
func (e Enum) AddValueDdl(value string) (string, bool) {
	position := slices.Index(e.Values, value)
	if position < 0 {
		return "", false
	}
	ddl := fmt.Sprintf("alter type %s add value if not exists %s", QuoteIdent(e.Name), QuoteLiteral(value))
	switch {
	case position > 0:
		ddl += " after " + QuoteLiteral(e.Values[position-1])
	case len(e.Values) > 1:
		ddl += " before " + QuoteLiteral(e.Values[1])
	}
	return ddl, true
}

// GenerateDdl creates the sequence with the settings it defines - its owner is set separately, once the owning table
// exists (OwnedByDdl)
func (s Sequence) GenerateDdl() string {
	var bld strings.Builder
	fmt.Fprintf(&bld, "create sequence %s", QuoteIdent(s.Name))
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
	return fmt.Sprintf("alter sequence %s owned by %s", QuoteIdent(s.Name), quoteQualified(s.Ownedby))
}

// AlterDdl brings every setting and the owner of an existing sequence in line with the definition - a setting that is
// not defined goes back to postgres' default. The sequence is not restarted, so start is left as it is
func (s Sequence) AlterDdl() string {
	var bld strings.Builder
	datatype := s.Datatype
	if datatype == "" {
		datatype = "bigint"
	}
	fmt.Fprintf(&bld, "alter sequence %s as %s", QuoteIdent(s.Name), datatype)
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
		fmt.Fprintf(&bld, " owned by %s", quoteQualified(s.Ownedby))
	} else {
		bld.WriteString(" owned by none")
	}
	return bld.String()
}

// GenerateDdl creates the materialized view, populated unless data is false - its indexes are created separately
func (m Matview) GenerateDdl() string {
	data := "with data"
	if m.Data != nil && !*m.Data {
		data = "with no data"
	}
	return fmt.Sprintf("create materialized view if not exists %s (%s) as %s %s", QuoteIdent(m.Name), quoteList(m.Columns, ","), m.Sql, data)
}

// ShadowDdl stands in for the materialized view in a shadow build, which creates everything as temporary objects -
// postgres does not allow a temporary materialized view. A temporary view under a scratch name has the same
// definition, and an empty temporary table with the materialized view's name and columns takes its indexes and is
// what other views select from
func (m Matview) ShadowDdl() []string {
	scratch := MatviewShadowPrefix + m.Name
	return []string{
		fmt.Sprintf("create or replace view %s (%s) as %s", QuoteIdent(scratch), quoteList(m.Columns, ","), m.Sql),
		fmt.Sprintf("create table %s as select * from %s with no data", QuoteIdent(m.Name), QuoteIdent(scratch)),
	}
}

// GenerateDdl creates (or replaces) the view
func (v View) GenerateDdl() string {
	var bld strings.Builder
	bld.WriteString("create or replace view ")
	bld.WriteString(QuoteIdent(v.Name))
	fmt.Fprintf(&bld, " (%s) as %s", quoteList(v.Columns, ","), v.Sql)
	return bld.String()
}

// AlterStmt adds the column to the table (action add) or drops it (drop)
func (col Column) AlterStmt(tablename string, action string) string {
	switch action {
	case "add":
		var bld strings.Builder
		fmt.Fprintf(&bld, addColTmpl, QuoteIdent(tablename), QuoteIdent(col.Name))
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
		return fmt.Sprintf(dropColTmpl, QuoteIdent(tablename), QuoteIdent(col.Name))
	}
	return ""
}

func (col Column) defaultBuilder() string {
	numericType := util.FromSlice([]string{"smallint", "integer", "bigint", "numeric", "decimal"})
	if col.Default != "" {
		if col.Dfltfunc || col.Datatype == "bool" || numericType.Has(col.Datatype) {
			return fmt.Sprintf(" default %s", col.Default)
		} else {
			return fmt.Sprintf(" default '%s'", strings.ReplaceAll(col.Default, "'", "''")) // a string literal
		}
	}
	return ""
}

// AlterStmt adds the foreign key to the table (action add) or drops it (drop)
func (fk ForeignKey) AlterStmt(tableName string, action string) string {
	var bld strings.Builder
	fmt.Fprintf(&bld, fkddlTmpl, QuoteIdent(tableName), action, QuoteIdent(fk.Name), quoteList(fk.Columns, ", "), QuoteIdent(fk.Reference.Table), quoteList(fk.Reference.Columns, ", "))
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

// GenerateDdl creates the index on the table - its ddl when it has one
func (idx Index) GenerateDdl(tableName string) string {
	if len(idx.Ddl) == 0 {
		var unique string
		if idx.Unique {
			unique = "unique"
		}
		idx.Ddl = fmt.Sprintf(idxddlTmpl, unique, QuoteIdent(idx.Name), QuoteIdent(tableName), quoteList(idx.Columns, ", "))
	}
	return idx.Ddl
}

// GenerateDdl adds the constraint to the table
func (cons Constraint) GenerateDdl(tableName string) string {
	var bld strings.Builder
	fmt.Fprintf(&bld, "alter table %s add constraint %s ", QuoteIdent(tableName), QuoteIdent(cons.Name))
	switch cons.Type {
	case "unique":
		fmt.Fprintf(&bld, "unique (%s)", quoteList(cons.Columns, ","))
	case "check":
		fmt.Fprintf(&bld, "check (%s)", cons.Condition)
	}
	return bld.String()
}

// Ddl generates the foreign key delta's statement from its own definition
func (fk FkDelta) Ddl() (string, error) {
	return fk.Definition.AlterStmt(fk.Tablename, fk.Action), nil
}

// StatementType is how the delta is recorded in the history, e.g. "delta add fk"
func (fk FkDelta) StatementType() string {
	return fmt.Sprintf("delta %s %s", fk.Action, fk.Object)
}

// Describe names what the delta changes, e.g. "add documents.documents_project_fk"
func (fk FkDelta) Describe() string {
	return fmt.Sprintf("%s %s.%s", fk.Action, fk.Tablename, fk.Definition.Name)
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
	fmt.Fprintf(&builder, "alter table %s alter column %s ", QuoteIdent(col.Reference), QuoteIdent(col.Name))
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

// StatementType is how the delta is recorded in the history, e.g. "delta alter column"
func (col ColumnDelta) StatementType() string {
	return fmt.Sprintf("delta %s %s", col.Action, col.Object)
}

// Describe names what the delta changes, e.g. "alter documents.priority"
func (col ColumnDelta) Describe() string {
	return fmt.Sprintf("%s %s.%s", col.Action, col.Reference, col.Name)
}

// Error describes the missing definition
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
		switch sd.Action {
		case "add":
			t, err := ParseTableByName(sd.Name)
			if err != nil {
				return "", fmt.Errorf("unable to parse the table %s: %w", sd.Name, err)
			}
			return t.GenerateDdl(), nil
		case "drop":
			return fmt.Sprintf(dropTableTmpl, QuoteIdent(sd.Name)), nil
		}
	case "view":
		switch sd.Action {
		case "add":
			v, err := ParseViewByName(sd.Name)
			if err != nil {
				return "", fmt.Errorf("unable to parse the view %s: %w", sd.Name, err)
			}
			return v.GenerateDdl(), nil
		case "drop":
			return fmt.Sprintf("drop view if exists %s", QuoteIdent(sd.Name)), nil
		}
	case "column":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropColTmpl, QuoteIdent(sd.Reference), QuoteIdent(sd.Name)), nil
		case "rename":
			return fmt.Sprintf(colRenameTmpl, QuoteIdent(sd.Reference), QuoteIdent(sd.Name), QuoteIdent(sd.NewName)), nil
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
			return fmt.Sprintf(dropFkTmpl, QuoteIdent(sd.Reference), QuoteIdent(sd.Name)), nil
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
			return fmt.Sprintf(dropFkTmpl, QuoteIdent(sd.Reference), QuoteIdent(sd.Name)), nil
		case "add":
			if len(sd.Reference) > 0 {
				t, err := sd.referenceTable()
				if err != nil {
					return "", err
				}
				if len(t.PrimaryKey) == 0 {
					return "", missingDefinition("Table %s did not have a primary key definition", sd.Reference)
				}
				return fmt.Sprintf("alter table %s add constraint %s primary key (%s)", QuoteIdent(sd.Reference), QuoteIdent(sd.Name), quoteList(t.PrimaryKey, ", ")), nil
			}
		}
	case "trigger":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropTriggerTmpl, QuoteIdent(sd.Name), QuoteIdent(sd.Reference)), nil
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
				return "", fmt.Errorf("unable to open the sql file for %s - expected %s: %w", sd.Name, strings.Join(FunctionFilePaths("", sd.Name), " or "), err)
			}
			return string(dat), nil
		}
	case "index":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf(dropIndexTmpl, QuoteIdent(sd.Name)), nil
		case "add":
			if len(sd.Reference) > 0 {
				indexes, err := sd.referenceIndexes()
				if err != nil {
					return "", err
				}
				for _, idx := range indexes {
					if idx.Name == sd.Name {
						return idx.GenerateDdl(sd.Reference), nil
					}
				}
				return "", missingDefinition("Table %s did not have a index definition matching %s", sd.Reference, sd.Name)
			}
		}
	case "matview":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf("drop materialized view if exists %s", QuoteIdent(sd.Name)), nil
		case "refresh":
			if sd.Concurrently {
				return fmt.Sprintf("refresh materialized view concurrently %s", QuoteIdent(sd.Name)), nil
			}
			return fmt.Sprintf("refresh materialized view %s", QuoteIdent(sd.Name)), nil
		case "add":
			mv, err := ParseMatviewByName(sd.Name)
			if err != nil {
				return "", fmt.Errorf("unable to parse the materialized view %s: %w", sd.Name, err)
			}
			return mv.GenerateDdl(), nil
		}
	case "enum":
		switch sd.Action {
		case "drop":
			return fmt.Sprintf("drop type if exists %s", QuoteIdent(sd.Name)), nil
		case "add", "alter":
			e, err := ParseEnumByName(sd.Name)
			if err != nil {
				return "", fmt.Errorf("unable to parse the enum %s: %w", sd.Name, err)
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
			return fmt.Sprintf("drop sequence if exists %s", QuoteIdent(sd.Name)), nil
		case "add", "alter":
			seq, err := ParseSequenceByName(sd.Name)
			if err != nil {
				return "", fmt.Errorf("unable to parse the sequence %s: %w", sd.Name, err)
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
			return fmt.Sprintf("alter table %s drop constraint if exists %s", QuoteIdent(sd.Reference), QuoteIdent(sd.Name)), nil
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
			return "", fmt.Errorf("unable to parse the table %s: %w", sd.Name, err)
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
			return "", fmt.Errorf("unable to parse the view %s: %w", sd.Name, err)
		}
		comment = v.Comment
	case "enum":
		e, err := ParseEnumByName(sd.Name)
		if err != nil {
			return "", fmt.Errorf("unable to parse the enum %s: %w", sd.Name, err)
		}
		comment = e.Comment
	case "sequence":
		seq, err := ParseSequenceByName(sd.Name)
		if err != nil {
			return "", fmt.Errorf("unable to parse the sequence %s: %w", sd.Name, err)
		}
		comment = seq.Comment
	case "matview":
		mv, err := ParseMatviewByName(sd.Name)
		if err != nil {
			return "", fmt.Errorf("unable to parse the materialized view %s: %w", sd.Name, err)
		}
		comment = mv.Comment
	default:
		return "", missingDefinition("cannot comment on %q - use table, column, view, enum, sequence or matview", sd.On)
	}
	if comment == "" {
		return "", missingDefinition("%s %s has no comment in its state file", sd.On, sd.commentTarget())
	}
	return comment, nil
}

// CommentDdl sets the comment on a table, column (target is table.column), view, enum, sequence or matview - an empty
// comment removes it. The target's names are quoted where they need it.
// The comment is quoted as a string literal
func CommentDdl(on string, target string, comment string) string {
	literal := "null"
	if comment != "" {
		literal = QuoteLiteral(comment)
	}
	switch on {
	case "enum":
		on = "type" // an enum is a type to postgres
	case "matview":
		on = "materialized view"
	}
	return fmt.Sprintf("comment on %s %s is %s", on, quoteQualified(target), literal)
}

// QuoteLiteral quotes text as a postgres string literal. With standard_conforming_strings on (the default since
// postgres 9.1) backslashes in a literal are ordinary characters, so only single quotes are doubled
func QuoteLiteral(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}

// StatementType is how the delta is recorded in the history, e.g. "delta add index"
func (sd SimpleDelta) StatementType() string {
	return fmt.Sprintf("delta %s %s", sd.Action, sd.Object)
}

// Describe names what the delta changes, e.g. "add documents.documents_type_idx"
func (sd SimpleDelta) Describe() string {
	var bld strings.Builder
	bld.WriteString(sd.Action)
	bld.WriteString(" ")
	if sd.Object == "comment" {
		fmt.Fprintf(&bld, "comment on %s ", sd.On)
	}
	if sd.Object == "matview" && sd.Action == "refresh" && sd.Concurrently {
		fmt.Fprintf(&bld, "%s (concurrently)", sd.Name)
		return bld.String()
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

// Describe is the delta's description
func (sd SqlDelta) Describe() string {
	return sd.Description
}

// StatementType is how the delta is recorded in the history
func (sd SqlDelta) StatementType() string {
	return "Standalone SQL delta"
}

// Ddl returns the script delta's first statement, or its script file's contents - plan uses Statements for every
// statement
func (sd SqlDelta) Ddl() (string, error) {
	if len(sd.Script) > 0 {
		return readScript(sd.Script)
	}
	if len(sd.Sql) == 0 {
		return "", missingDefinition("the script delta %q has no sql or script", sd.Description)
	}
	return sd.Sql[0], nil
}

// GenerateDdl creates the table with its columns and primary key - its foreign keys, indexes, constraints and
// triggers are created separately
func (t Table) GenerateDdl() string {
	var bld strings.Builder
	ignorePk := false
	fmt.Fprintf(&bld, "create table if not exists %s (", QuoteIdent(t.Name))
	for i, col := range t.Columns {
		if i > 0 {
			bld.WriteString(",")
		}
		fmt.Fprintf(&bld, "%s %s", QuoteIdent(col.Name), col.Datatype)
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
		fmt.Fprintf(&bld, " , constraint %s primary key (%s)", QuoteIdent(t.Name+"_pk"), quoteList(t.PrimaryKey, ", "))
	}
	bld.WriteString(") ")

	return bld.String()
}

// GenerateFkDdl adds the table's foreign keys - once every table exists
func (t Table) GenerateFkDdl() []string {
	fks := make([]string, 0)
	for _, fk := range t.ForeignKeys {
		fks = append(fks, fk.AlterStmt(t.Name, "add"))
	}
	return fks
}
