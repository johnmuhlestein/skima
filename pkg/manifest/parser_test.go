package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// const tableJson = "{\"name\":\"documents\",\"columns\":[{\"name\":\"id\",\"datatype\":\"uuid\",\"nullable\":false},{\"name\":\"document_type\",\"datatype\":\"varchar\",\"nullable\":true},{\"name\":\"file_location\",\"datatype\":\"varchar\",\"nullable\":true},{\"name\":\"upload_timestamp\",\"datatype\":\"timestamp\",\"nullable\":false,\"default\":\"NOW()\",\"dfltfunc\":true},{\"name\":\"project_id\",\"datatype\":\"uuid\",\"nullable\":false}],\"primary-key\":[\"id\"],\"foreign-keys\":[{\"name\":\"doc_project_fk\",\"columns\":[\"project_id\"],\"reference\":{\"table\":\"projects\",\"columns\":[\"id\"]},\"deleterule\":\"no action\"}]}"
const tableColumnDefaults = "{\"name\":\"annotation\",\"columns\":[{\"name\":\"id\",\"datatype\":\"uuid\",\"nullable\":false},{\"name\":\"nullable_varchar\",\"datatype\":\"varchar(100)\",\"nullable\":true},{\"name\":\"nullable_int_dflt\",\"datatype\":\"integer\",\"nullable\":true,\"default\":\"0\"},{\"name\":\"notnull_decimal_dflt\",\"datatype\":\"decimal\",\"nullable\":false,\"default\":\"100.11\"},{\"name\":\"notnull_numeric_dflt\",\"datatype\":\"numeric\",\"nullable\":false,\"default\":\"100.22\"},{\"name\":\"notnull_smallint_dflt\",\"datatype\":\"smallint\",\"nullable\":false,\"default\":\"2\"},{\"name\":\"notnull_bigint_dflt\",\"datatype\":\"bigint\",\"nullable\":false,\"default\":\"98765432123\"},{\"name\":\"nullable_bool\",\"datatype\":\"bool\",\"nullable\":true},{\"name\":\"nullable_bool_dflt\",\"datatype\":\"bool\",\"nullable\":true,\"default\":\"false\"},{\"name\":\"notnull_bool_dflt\",\"datatype\":\"bool\",\"nullable\":false,\"default\":\"true\"},{\"name\":\"notnull_varchar_dflt\",\"datatype\":\"varchar\",\"nullable\":false,\"default\":\"defaultval\"},{\"name\":\"notnull_ts_dflt_func\",\"datatype\":\"timestamp\",\"nullable\":false,\"default\":\"NOW()\",\"dfltfunc\":true}],\"primary-key\":[\"id\"]}"
const tableOnePk = "{\"name\":\"tester\",\"columns\":[{\"name\":\"id\",\"datatype\":\"uuid\",\"nullable\":false},{\"name\":\"nullable_varchar\",\"datatype\":\"varchar(100)\",\"nullable\":true}],\"primary-key\":[\"id\"]}"
const tableTwoPk = "{\"name\":\"tester\",\"columns\":[{\"name\":\"id\",\"datatype\":\"uuid\",\"nullable\":false},{\"name\":\"id_two\",\"datatype\":\"varchar(100)\",\"nullable\":false}],\"primary-key\":[\"id\",\"id_two\"]}"
const masterTesterTable = "{\"name\":\"tester\",\"columns\":[{\"name\":\"id\",\"datatype\":\"uuid\",\"nullable\":false},{\"name\":\"col_txt_nnull\",\"datatype\":\"text\",\"nullable\":false},{\"name\":\"col_txt_dflt\",\"datatype\":\"text\",\"nullable\":false,\"default\":\"maybe\"},{\"name\":\"col_rename_me\",\"datatype\":\"uuid\",\"nullable\":false},{\"name\":\"col_ts_nnull_dfltfunc\",\"datatype\":\"timestamptz\",\"nullable\":false,\"default\":\"NOW()\",\"dfltfunc\":true},{\"name\":\"col_numeric_dflt\",\"datatype\":\"numeric\",\"nullable\":false,\"default\":\"100\"},{\"name\":\"col_text_nullable\",\"datatype\":\"text\",\"nullable\":true},{\"name\":\"col_bool_nnull_dflt\",\"datatype\":\"bool\",\"nullable\":false,\"default\":\"false\"},{\"name\":\"col_selfref\",\"datatype\":\"uuid\",\"nullable\":true},{\"name\":\"col_otherref_indexed\",\"datatype\":\"uuid\",\"nullable\":true},{\"name\":\"col_indexed_unique\",\"datatype\":\"numeric\",\"nullable\":false}],\"primary-key\":[\"id\"],\"constraints\":[{\"name\":\"tester_name_col_txt_nnull_unique\",\"type\":\"unique\",\"columns\":[\"col_txt_nnull\"]}],\"indexes\":[{\"name\":\"tester_col_indexed_unique_idx\",\"columns\":[\"col_indexed_unique\"],\"unique\":true},{\"name\":\"tester_col_otherref_indexed_idx\",\"columns\":[\"col_otherref_indexed\"]}],\"foreign-keys\":[{\"name\":\"tester_col_otherref_indexed_tester2_fk\",\"columns\":[\"col_otherref_indexed\"],\"reference\":{\"table\":\"tester2\",\"columns\":[\"id\"]},\"deleterule\":\"cascade\"}],\"triggers\":[{\"name\":\"tester_col_ts_nnull_dfltfunc_bu_trg\",\"ddl\":\"create trigger tester_col_ts_nnull_dfltfunc_bu_trg before update on tester for each row execute function update_last_modified_at()\"}]}"
const tableWithFk = "{\"name\":\"projects\",\"columns\":[{\"name\":\"id\",\"datatype\":\"uuid\",\"nullable\":false},{\"name\":\"name\",\"datatype\":\"varchar\",\"nullable\":true},{\"name\":\"parent_id\",\"datatype\":\"uuid\",\"nullable\":true},{\"name\":\"model_owner\",\"datatype\":\"bool\",\"nullable\":true},{\"name\":\"solution_id\",\"datatype\":\"uuid\",\"nullable\":true}],\"primary-key\":[\"id\"],\"foreign-keys\":[{\"name\":\"projects_solution_id_fkey\",\"columns\":[\"solution_id\"],\"reference\":{\"table\":\"solutions\",\"columns\":[\"id\"]},\"deleterule\":\"no action\"}]}"
const tableColumnIdentity = "{\"name\":\"tester\",\"columns\":[{\"name\":\"seq_id\",\"datatype\":\"integer\",\"nullable\":false,\"primarykey\":true,\"generate\":\"always\"},{\"name\":\"nullable_varchar\",\"datatype\":\"varchar(100)\",\"nullable\":true}]}"
const tableColumnIdentityPk = "{\"name\":\"tester\",\"columns\":[{\"name\":\"seq_id\",\"datatype\":\"integer\",\"nullable\":false,\"primarykey\":true,\"generate\":\"always\"},{\"name\":\"nullable_varchar\",\"datatype\":\"varchar(100)\",\"nullable\":true}],\"primary-key\":[\"seq_id\"]}"
const masterScript = `CREATE OR REPLACE FUNCTION update_last_modified_at()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN
	NEW.last_modified_at = now();
	RETURN NEW;
END;
$function$
;`

type osReadTableStateMock struct{}
type osReadScriptStateMock struct{}

func (orw osReadTableStateMock) readFile(name string) ([]byte, error) {
	return []byte(masterTesterTable), nil
}

func (orw osReadTableStateMock) readDir(name string) ([]os.DirEntry, error) {
	b, e := os.ReadDir(name)
	return b, e
}

func (orw osReadScriptStateMock) readFile(name string) ([]byte, error) {
	return []byte(masterScript), nil
}

func (orw osReadScriptStateMock) readDir(name string) ([]os.DirEntry, error) {
	b, e := os.ReadDir(name)
	return b, e
}

func TestParseTable(t *testing.T) {
	tbl, err := parseTable([]byte(tableColumnDefaults))
	if err != nil {
		t.Error("Error parsing valid json into a Table object:", err)
	} else {
		if tbl.Name != "annotation" {
			t.Errorf("Failed to parse JSON into a table object expected name=documents but got %s", tbl.Name)
		}
		if len(tbl.Columns) != 12 {
			t.Errorf("Failed to parse JSON into a Table object - expected 5 columns but only found %d", len(tbl.Columns))
		}
	}
	t.Run("column1", testColumnFunc(tbl.Columns[0], map[string]any{"name": "id", "datatype": "uuid", "nullable": false, "builder": ""}))
	t.Run("column2", testColumnFunc(tbl.Columns[1], map[string]any{"name": "nullable_varchar", "datatype": "varchar(100)", "nullable": true, "builder": ""}))
	t.Run("column3", testColumnFunc(tbl.Columns[2], map[string]any{"name": "nullable_int_dflt", "datatype": "integer", "nullable": true, "default": "0", "builder": " default 0"}))
	t.Run("column4", testColumnFunc(tbl.Columns[3], map[string]any{"name": "notnull_decimal_dflt", "datatype": "decimal", "nullable": false, "default": "100.11", "builder": " default 100.11"}))
	t.Run("column5", testColumnFunc(tbl.Columns[4], map[string]any{"name": "notnull_numeric_dflt", "datatype": "numeric", "nullable": false, "default": "100.22", "builder": " default 100.22"}))
	t.Run("column6", testColumnFunc(tbl.Columns[5], map[string]any{"name": "notnull_smallint_dflt", "datatype": "smallint", "nullable": false, "default": "2", "builder": " default 2"}))
	t.Run("column7", testColumnFunc(tbl.Columns[6], map[string]any{"name": "notnull_bigint_dflt", "datatype": "bigint", "nullable": false, "default": "98765432123", "builder": " default 98765432123"}))
	t.Run("column8", testColumnFunc(tbl.Columns[7], map[string]any{"name": "nullable_bool", "datatype": "bool", "nullable": true, "builder": ""}))
	t.Run("column9", testColumnFunc(tbl.Columns[8], map[string]any{"name": "nullable_bool_dflt", "datatype": "bool", "nullable": true, "default": "false", "builder": " default false"}))
	t.Run("column10", testColumnFunc(tbl.Columns[9], map[string]any{"name": "notnull_bool_dflt", "datatype": "bool", "nullable": false, "default": "true", "builder": " default true"}))
	t.Run("column11", testColumnFunc(tbl.Columns[10], map[string]any{"name": "notnull_varchar_dflt", "datatype": "varchar", "nullable": false, "default": "defaultval", "builder": " default 'defaultval'"}))
	t.Run("column12", testColumnFunc(tbl.Columns[11], map[string]any{"name": "notnull_ts_dflt_func", "datatype": "timestamp", "nullable": false, "default": "NOW()", "dfltfunc": true, "builder": " default NOW()"}))
}

func testColumnFunc(col Column, expected map[string]any) func(*testing.T) {
	return func(t *testing.T) {
		if actual := col.Name; actual != expected["name"].(string) {
			t.Errorf("Expected column name %s. Actual value: %s", expected["name"].(string), actual)
		}
		if actual := col.Datatype; actual != expected["datatype"].(string) {
			t.Errorf("Expected column datatype %s. Actual value: %s", expected["datatype"].(string), actual)
		}
		if actual := col.Nullable; actual != expected["nullable"].(bool) {
			t.Errorf("Expected column nullable %t. Actual value %t", expected["nullable"].(bool), actual)
		}
		if val, ok := expected["default"]; ok {
			if actual := col.Default; actual != val.(string) {
				t.Errorf("Expected column default %s. Actual value %s", val.(string), actual)
			}
		}
		if val, ok := expected["dftlfunc"]; ok {
			if actual := col.Dfltfunc; actual != val.(bool) {
				t.Errorf("Expected column dfltfunc %t. Actual value %t", val.(bool), actual)
			}
		}
		if actual := col.defaultBuilder(); actual != expected["builder"].(string) {
			t.Errorf("Expected column defalut builder '%s'. Actual value '%s'", expected["builder"].(string), actual)
		}
	}
}

func TestTable_GenerateFkDdl(t *testing.T) {
	tbl, _ := parseTable([]byte(tableWithFk))
	var expected = "alter table projects add constraint projects_solution_id_fkey foreign key (solution_id) references solutions (id) on delete no action"
	fks := tbl.GenerateFkDdl()
	if fks[0] != expected {
		t.Errorf("FK DDL mismatch EXPECTED: %s  ACTUAL: %s", expected, fks[0])
	}
}

func TestPrimaryKey(t *testing.T) {
	tbl, _ := parseTable([]byte(tableOnePk))
	tbl2, _ := parseTable([]byte(tableTwoPk))
	t.Run("onecolumn", testPkFunc(tbl, 1, "tester_pk primary key (id))"))
	t.Run("twocolumn", testPkFunc(tbl2, 2, "tester_pk primary key (id, id_two))"))
}

func TestIdentityColumn(t *testing.T) {
	expectedDdl := "create table if not exists tester (seq_id integer primary key generated always as identity,nullable_varchar varchar(100))"
	tbl, _ := parseTable([]byte(tableColumnIdentity))
	tbl2, _ := parseTable([]byte(tableColumnIdentityPk))
	t.Run("ddl no pk", testIdentityFunc(tbl, expectedDdl))
	t.Run("ddl with pk", testIdentityFunc(tbl2, expectedDdl))
}

func testPkFunc(tbl Table, expectedLen int, expectedDdl string) func(*testing.T) {
	return func(t *testing.T) {
		if actual := len(tbl.PrimaryKey); actual != expectedLen {
			t.Errorf("Expected PK column count %d. Actual value %d", expectedLen, actual)
		}
		ddl := tbl.GenerateDdl()
		ddl = strings.TrimRight(ddl, " ")
		_, pkddl, _ := strings.Cut(ddl, ", constraint ")
		if pkddl != expectedDdl {
			t.Errorf("Expected PK DDL [%s]. Actual value [%s]", expectedDdl, pkddl)
		}
	}
}

func testIdentityFunc(tbl Table, expectedDdl string) func(*testing.T) {
	return func(t *testing.T) {
		ddl := tbl.GenerateDdl()
		ddl = strings.TrimRight(ddl, " ")
		if ddl != expectedDdl {
			t.Errorf("Expected DDL for identity column [%s]. Actual value [%s]", expectedDdl, ddl)
		}
	}
}
func TestForeignKey_AlterStmt(t *testing.T) {
	fk1 := ForeignKey{Name: "testing_fk", Columns: []string{"parent_id"}, Reference: PrimaryKeyRef{"parent_table", []string{"id"}}}
	t.Run("DefaultUpdateDefaultDelete", testForeignKey_AlterStmtFunc(fk1, "child_table", "add", "alter table child_table add constraint testing_fk foreign key (parent_id) references parent_table (id)"))
	fk2 := ForeignKey{Name: "testing_fk", Columns: []string{"parent_id"}, Reference: PrimaryKeyRef{"parent_table", []string{"id"}}, DeleteRule: "cascade"}
	t.Run("DefaultUpdateCascadeDelete", testForeignKey_AlterStmtFunc(fk2, "child_table", "add", "alter table child_table add constraint testing_fk foreign key (parent_id) references parent_table (id) on delete cascade"))
	fk3 := ForeignKey{Name: "testing_fk", Columns: []string{"parent_id"}, Reference: PrimaryKeyRef{"parent_table", []string{"id"}}, UpdateRule: "cascade"}
	t.Run("CascadeUpdateDefaultUpdate", testForeignKey_AlterStmtFunc(fk3, "child_table", "add", "alter table child_table add constraint testing_fk foreign key (parent_id) references parent_table (id) on update cascade"))
	fk4 := ForeignKey{Name: "testing_fk", Columns: []string{"parent_id"}, Reference: PrimaryKeyRef{"parent_table", []string{"id"}}, DeleteRule: "cascade", UpdateRule: "cascade"}
	t.Run("CascadeUpdateCascadeUpdate", testForeignKey_AlterStmtFunc(fk4, "child_table", "add", "alter table child_table add constraint testing_fk foreign key (parent_id) references parent_table (id) on update cascade on delete cascade"))
}

func testForeignKey_AlterStmtFunc(fk ForeignKey, tbl string, action string, expected string) func(*testing.T) {
	return func(t *testing.T) {
		actual := fk.AlterStmt(tbl, action)
		if actual != expected {
			t.Errorf("FK Alter statement did not match. EXPECTED: %s  ACTUAL: %s", expected, actual)
		}
	}
}

func TestFkDelta_GenerateDdl(t *testing.T) {
	expected := "alter table child_table add constraint testing_fk foreign key (parent_id) references parent_table (id) on delete cascade"
	fk := ForeignKey{Name: "testing_fk", Columns: []string{"parent_id"}, DeleteRule: "cascade", Reference: PrimaryKeyRef{"parent_table", []string{"id"}}}
	delta := FkDelta{Object: "fk", Tablename: "child_table", Action: "add", Definition: fk}
	fkDdl := delta.GenerateDdl()
	if fkDdl != expected {
		t.Errorf("Generated FK is not valid:\n EXPECTED: %s\n ACTUAL: %s\n", expected, fkDdl)
	}
}

func TestColumnAlterDdl(t *testing.T) {
	t.Run("addColumn1", testColumnAlterFunc(Column{Name: "col1", Datatype: "varchar", Nullable: true}, "testtable", "add", "alter table testtable add column if not exists col1 varchar"))
	t.Run("addColumn2", testColumnAlterFunc(Column{Name: "col1", Datatype: "varchar", Nullable: false}, "testtable", "add", "alter table testtable add column if not exists col1 varchar not null"))
	t.Run("addColumn3", testColumnAlterFunc(Column{Name: "col2", Datatype: "numeric", Nullable: false, Default: "10"}, "testtable", "add", "alter table testtable add column if not exists col2 numeric not null default 10"))
	t.Run("addColumn4", testColumnAlterFunc(Column{Name: "col4", Datatype: "varchar", Nullable: false, Default: "magic"}, "testtable", "add", "alter table testtable add column if not exists col4 varchar not null default 'magic'"))
	t.Run("addColumn5", testColumnAlterFunc(Column{Name: "col5", Datatype: "timestamp", Nullable: false, Default: "NOW()", Dfltfunc: true}, "testtable", "add", "alter table testtable add column if not exists col5 timestamp not null default NOW()"))
	t.Run("dropColumn1", testColumnAlterFunc(Column{Name: "col1", Datatype: "varchar", Nullable: true}, "testtable", "drop", "alter table testtable drop column if exists col1"))
}

func testColumnAlterFunc(col Column, tableName string, action string, expected string) func(*testing.T) {
	return func(t *testing.T) {
		actual := col.AlterStmt(tableName, action)
		if actual != expected {
			t.Errorf("Expected alter statement: %s. Actual value %s", expected, actual)
		}
	}
}

func TestView_GenerateDdl(t *testing.T) {
	expected := "create or replace view testview (col1, col2, col3) as select col1, altcol, datecol from testtable"
	view := View{Name: "testview", Columns: []string{"col1, col2, col3"}, Sql: "select col1, altcol, datecol from testtable"}
	ddl := view.GenerateDdl()
	if ddl != expected {
		t.Errorf("Error generating view ddl - Expected: %s  Actual: %s", expected, ddl)
	}
}

func TestConstraint_GenerateDdl(t *testing.T) {
	t.Run("UniqueConstraintSingleColumn", testConstraintGenerateDdlFunc(Constraint{Name: "test_constraint", Type: "unique", Columns: []string{"col1"}}, "test_table", "alter table test_table add constraint test_constraint unique (col1)"))
	t.Run("UniqueConstraintMultiColumn", testConstraintGenerateDdlFunc(Constraint{Name: "test_constraint", Type: "unique", Columns: []string{"col1", "col2"}}, "test_table", "alter table test_table add constraint test_constraint unique (col1,col2)"))
	t.Run("CheckConstraintText", testConstraintGenerateDdlFunc(Constraint{Name: "test_constraint", Type: "check", Condition: "text_col in ('val1','val2','val3')"}, "test_table", "alter table test_table add constraint test_constraint check (text_col in ('val1','val2','val3'))"))
	t.Run("CheckConstraintValGreater", testConstraintGenerateDdlFunc(Constraint{Name: "test_constraint", Type: "check", Condition: "num_col > 10"}, "test_table", "alter table test_table add constraint test_constraint check (num_col > 10)"))
}

func testConstraintGenerateDdlFunc(cons Constraint, tableName string, expected string) func(t *testing.T) {
	return func(t *testing.T) {
		actual := cons.GenerateDdl(tableName)
		if actual != expected {
			t.Errorf("Error generating constraint DDL - EXPECTED: %s  ACTUAL: %s", expected, actual)
		}
	}
}

func TestSimpleDelta_GenerateDdlDrop(t *testing.T) {
	reader = osReadTableStateMock{}
	t.Run("DropTable", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "test_table", Action: "drop", Object: "table"}, "drop table test_table"))
	t.Run("DropView", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "test_view", Action: "drop", Object: "view"}, "drop view if exists test_view"))
	t.Run("DropColumn", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "test_column", Action: "drop", Object: "column", Reference: "test_table"}, "alter table test_table drop column if exists test_column"))
	t.Run("DropForeignKey", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "test_foreign_key", Action: "drop", Object: "fk", Reference: "test_table"}, "alter table test_table drop constraint test_foreign_key"))
	t.Run("DropPrimaryKey", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "test_primary_key", Action: "drop", Object: "pk", Reference: "test_table"}, "alter table test_table drop constraint test_primary_key"))
	t.Run("AddPrimaryKey", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "tester_pk", Action: "add", Object: "pk", Reference: "tester"}, "alter table tester add constraint tester_pk primary key (id)"))
	t.Run("DropTrigger", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "test_trigger", Action: "drop", Object: "trigger", Reference: "test_table"}, "drop trigger if exists test_trigger on test_table"))
	t.Run("DropIndex", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "test_index", Action: "drop", Object: "index"}, "drop index if exists test_index cascade"))
	t.Run("DropFunction", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "test_function", Action: "drop", Object: "function"}, "drop function if exists test_function"))
	t.Run("DropConstraint", testSimpleDelta_GenerateDdlFunc(SimpleDelta{Name: "test_constraint", Action: "drop", Object: "constraint", Reference: "test_table"}, "alter table test_table drop constraint if exists test_constraint"))
}

func testSimpleDelta_GenerateDdlFunc(sd SimpleDelta, expected string) func(t *testing.T) {
	return func(t *testing.T) {
		actual := sd.GenerateDdl()
		if actual != expected {
			t.Errorf("Error generating Drop statement: EXPECTED: %s  ACTUAL: %s", expected, actual)
		}
	}
}

func TestColumnDelta_GenerateDdl(t *testing.T) {
	reader = osReadTableStateMock{}
	t.Run("alterColumnDataType", testColumnDelta_GenerateDdlFunc(ColumnDelta{Name: "col_txt_nnull", Object: "column", Action: "alter", Reference: "tester", Mods: ColumnAlterAttributes{Datatype: true}}, "alter table tester alter column col_txt_nnull set data type text"))
	t.Run("alterColumnNotNull", testColumnDelta_GenerateDdlFunc(ColumnDelta{Name: "col_txt_nnull", Object: "column", Action: "alter", Reference: "tester", Mods: ColumnAlterAttributes{Nullable: true}}, "alter table tester alter column col_txt_nnull set NOT NULL"))
	t.Run("alterColumnNull", testColumnDelta_GenerateDdlFunc(ColumnDelta{Name: "col_text_nullable", Object: "column", Action: "alter", Reference: "tester", Mods: ColumnAlterAttributes{Nullable: true}}, "alter table tester alter column col_text_nullable drop NOT NULL"))
	t.Run("alterColumnDefaultWithFunc", testColumnDelta_GenerateDdlFunc(ColumnDelta{Name: "col_ts_nnull_dfltfunc", Object: "column", Action: "alter", Reference: "tester", Mods: ColumnAlterAttributes{Default: true}}, "alter table tester alter column col_ts_nnull_dfltfunc set default NOW()"))
	t.Run("alterColumnDefaultBool", testColumnDelta_GenerateDdlFunc(ColumnDelta{Name: "col_bool_nnull_dflt", Object: "column", Action: "alter", Reference: "tester", Mods: ColumnAlterAttributes{Default: true}}, "alter table tester alter column col_bool_nnull_dflt set default false"))
	t.Run("alterColumnDefaultNumeric", testColumnDelta_GenerateDdlFunc(ColumnDelta{Name: "col_numeric_dflt", Object: "column", Action: "alter", Reference: "tester", Mods: ColumnAlterAttributes{Default: true}}, "alter table tester alter column col_numeric_dflt set default 100"))
	t.Run("alterColumnDropDefault", testColumnDelta_GenerateDdlFunc(ColumnDelta{Name: "col_text_nullable", Object: "column", Action: "alter", Reference: "tester", Mods: ColumnAlterAttributes{Default: true}}, "alter table tester alter column col_text_nullable drop default"))
}

func testColumnDelta_GenerateDdlFunc(delta ColumnDelta, expected string) func(t *testing.T) {
	return func(t *testing.T) {
		actual := delta.GenerateDdl()
		if actual != expected {
			t.Errorf("Error generating ddl for the ColumnDelta: EXPECTED: %s  ACTUAL: %s", expected, actual)
		}
	}
}

func TestSimpleDelta_Hooks(t *testing.T) {
	simpleDeltaJson := "{\"pre\":{\"sql\":[\"update content_maps set col1 ='dfltval' where col1 is null\"]},\"object\":\"column\",\"action\":\"add\",\"name\":\"term_group_id\",\"reference\":\"content_maps\",\"post\":{\"script\":\"content_maps_post-1_0_5.sql\"}}"
	simpleDelta := SimpleDelta{}

	err := json.Unmarshal([]byte(simpleDeltaJson), &simpleDelta)
	if err != nil {
		t.Errorf("Error parsing Delta JSON: %s", err)
	}

	if len(simpleDelta.Pre.Sql) > 0 {
		expected := "update content_maps set col1 ='dfltval' where col1 is null"
		if simpleDelta.Pre.Sql[0] != expected {
			t.Errorf("Error setting Pre Hook: EXPECTED: %s  ACTUAL: %s", expected, simpleDelta.Pre.Sql[0])
		}
	} else {
		t.Error("Error setting Pre Hook - no Sql values found in the array")
	}

	if len(simpleDelta.Post.Script) > 0 {
		expected := "content_maps_post-1_0_5.sql"
		if simpleDelta.Post.Script != expected {
			t.Errorf("Error setting Post Hook: EXPECTED: %s  ACTUAL: %s", expected, simpleDelta.Post.Script)
		}
	} else {
		t.Error("Error setting Post Hook - no Script value defined in the Post Hook")
	}
}

func TestSimpleDelta_HasHooks(t *testing.T) {
	t.Run("PreAndPostSql", testSimpleDelta_HasHooksFunc("{\"pre\":{\"sql\":[\"insert into content_maps (col1) values ('dfltval') where col1 is null\"]},\"object\":\"column\",\"action\":\"add\",\"name\":\"term_group_id\",\"reference\":\"content_maps\",\"post\":{\"sql\":[\"insert into content_maps (col1) values ('dfltval') where col1 is null\"]}}", true, true))
	t.Run("PreAndPostScript", testSimpleDelta_HasHooksFunc("{\"pre\":{\"script\":\"content_maps_post-1_0_5.sql\"},\"object\":\"column\",\"action\":\"add\",\"name\":\"term_group_id\",\"reference\":\"content_maps\",\"post\":{\"script\":\"content_maps_post-1_0_5.sql\"}}", true, true))
	t.Run("PreOnlySql", testSimpleDelta_HasHooksFunc("{\"pre\":{\"sql\":[\"insert into content_maps (col1) values ('dfltval') where col1 is null\"]},\"object\":\"column\",\"action\":\"add\",\"name\":\"term_group_id\",\"reference\":\"content_maps\"}", true, false))
	t.Run("PreOnlyScript", testSimpleDelta_HasHooksFunc("{\"pre\":{\"script\":\"content_maps_post-1_0_5.sql\"},\"object\":\"column\",\"action\":\"add\",\"name\":\"term_group_id\",\"reference\":\"content_maps\"}", true, false))
	t.Run("PostOnlySql", testSimpleDelta_HasHooksFunc("{\"object\":\"column\",\"action\":\"add\",\"name\":\"term_group_id\",\"reference\":\"content_maps\",\"post\":{\"sql\":[\"insert into content_maps (col1) values ('dfltval') where col1 is null\"]}}", false, true))
	t.Run("PostOnlyScript", testSimpleDelta_HasHooksFunc("{\"object\":\"column\",\"action\":\"add\",\"name\":\"term_group_id\",\"reference\":\"content_maps\",\"post\":{\"script\":\"content_maps_post-1_0_5.sql\"}}", false, true))
	t.Run("NoHooks", testSimpleDelta_HasHooksFunc("{\"object\":\"column\",\"action\":\"add\",\"name\":\"term_group_id\",\"reference\":\"content_maps\"}", false, false))
}

func testSimpleDelta_HasHooksFunc(deltaJson string, expectedPre bool, expectedPost bool) func(t *testing.T) {
	return func(t *testing.T) {
		simpleDelta := SimpleDelta{}

		err := json.Unmarshal([]byte(deltaJson), &simpleDelta)
		if err != nil {
			t.Errorf("Error parsing Delta JSON: %s", err)
		}

		if simpleDelta.HasPreHook() != expectedPre {
			t.Errorf("HasPreHook failure: EXPECTED %t  ACTUAL: %t", expectedPre, simpleDelta.HasPostHook())
		}

		if simpleDelta.HasPostHook() != expectedPost {
			t.Errorf("HasPostHook failure: EXPECTED %t  ACTUAL: %t", expectedPre, simpleDelta.HasPostHook())
		}
	}
}

func TestSqlDelta(t *testing.T) {
	t.Run("OneStatementNoBypass", testSqlDelta("{\"object\":\"script\",\"description\":\"remove Lack of Liquidity from term_definitions\",\"sql\":[\"delete from term_definitions where name = 'Lack of Liquidity'\"]}", 1, 0, "remove Lack of Liquidity from term_definitions"))
	t.Run("TwoStatementOneBypass", testSqlDelta("{\"object\":\"script\",\"description\":\"replace Settlement Type with Delivery Type\",\"sql\":[\"delete from term_group_terms where id in (select tgt.id from term_group_terms tgt join term_groups tg on tgt.term_group_id = tg.id join term_definitions td on tgt.term_id = td.id where tg.name = 'SEC-302963 (Unstructured)' and td.name = 'Settlement Type')\",\"insert into term_group_terms (id, term_group_id, term_id, form, list_index, is_required) select overlay(overlay(md5(random()::text || ':' || clock_timestamp()::text) placing '4' from 13) placing '8' from 17)::uuid, tg.id, td.id, 'UNIT', 32, true FROM term_groups tg CROSS JOIN term_definitions td WHERE tg.name = 'SEC-302963 (Unstructured)' AND td.name = 'Delivery Type'\"],\"bypass\":[\"23505\"]}", 2, 1, "replace Settlement Type with Delivery Type"))
	t.Run("SixStatementTwoBypass", testSqlDelta("{\"object\":\"script\",\"description\":\"Add new terms to term_group_terms\",\"sql\":[\"update table set x='newX' where y=1\",\"update table set x='newX' where y=2\",\"update table set x='newX' where y=3\",\"update table set x='newX' where y=4\",\"update table set x='newX' where y=5\",\"update table set x='newX' where y=6\"],\"bypass\":[\"23505\",\"23503\"]}", 6, 2, "Add new terms to term_group_terms"))
}

func testSqlDelta(deltaJson string, expectedSqlCount int, expectedBypassCount int, expectedDescribe string) func(t *testing.T) {
	return func(t *testing.T) {
		sqlDelta := SqlDelta{}
		err := json.Unmarshal([]byte(deltaJson), &sqlDelta)
		if err != nil {
			t.Errorf("Error parsing DeltaJSON: %s", err)
		}

		if sqlDelta.Describe() != expectedDescribe {
			t.Errorf("SqlDelta.Describe failure - EXPECTED: %s  ACTUAL: %s", expectedDescribe, sqlDelta.Describe())
		}

		if sqlLength := len(sqlDelta.Sql); sqlLength != expectedSqlCount {
			t.Errorf("Number of sql statements did not match - EXPECTED: %d  ACTUAL: %d", expectedSqlCount, sqlLength)
		}

		if bypassLength := len(sqlDelta.Bypass); bypassLength != expectedBypassCount {
			t.Errorf("Number of bypass statements did not match - EXPECTED: %d  ACTUAL: %d", expectedBypassCount, bypassLength)
		}
	}
}

func TestScriptDelta(t *testing.T) {
	reader = osReadScriptStateMock{}
	sqlDelta := SqlDelta{}
	err := json.Unmarshal([]byte("{\"object\":\"script\",\"description\":\"Add Trigger Function\",\"script\":\"update-delete-audit.sql'\"}"), &sqlDelta)
	if err != nil {
		t.Errorf("Error parsing DeltaJSON: %s", err)
	}

	if sqlDelta.GenerateDdl() != masterScript {
		t.Errorf("SqlDelta.GenerateDdl failure - EXPECTED: %s  ACTUAL: %s", masterScript, sqlDelta.GenerateDdl())
	}
}

func TestVersion(t *testing.T) {
	t.Run("no deltas directory", testVersionFunc(nil, false, "1.0.0"))
	t.Run("empty deltas directory", testVersionFunc([]string{}, true, "1.0.0"))
	t.Run("only v1.0.0", testVersionFunc([]string{"v1_0_0.json"}, true, "1.0.0"))
	t.Run("only below 1.0.0", testVersionFunc([]string{"v0_9_0.json"}, true, "0.9.0"))
	t.Run("mixed below and above 1.0.0", testVersionFunc([]string{"v0_9_0.json", "v1_0_2.json"}, true, "1.0.2"))
	t.Run("semantic not file name order", testVersionFunc([]string{"v1_0_9.json", "v1_0_10.json", "v1_0_2.json"}, true, "1.0.10"))
	t.Run("release beats pre-release", testVersionFunc([]string{"v1_0_2-rc_1.json", "v1_0_2.json"}, true, "1.0.2"))
	t.Run("ignores non-json files", testVersionFunc([]string{"v1_0_1.json", "v9_0_0.txt"}, true, "1.0.1"))
}

func testVersionFunc(files []string, makeDir bool, expected string) func(t *testing.T) {
	return func(t *testing.T) {
		reader = osReadWrapper{}
		workdir := t.TempDir()
		viper.Set("workdir", workdir)
		t.Cleanup(func() { viper.Set("workdir", "") })
		if makeDir {
			deltaDir := filepath.Join(workdir, "deltas")
			if err := os.Mkdir(deltaDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				if err := os.WriteFile(filepath.Join(deltaDir, f), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		if actual := Version().String(); actual != expected {
			t.Errorf("Version mismatch - EXPECTED: %s  ACTUAL: %s", expected, actual)
		}
	}
}

func TestParseChangeSet_ScriptDelta(t *testing.T) {
	reader = osReadWrapper{}
	path := filepath.Join(t.TempDir(), "v1_0_1.json")
	changeset := `{"description":"script deltas","deltas":[
		{"object":"script","description":"from a file","script":"backfill.sql"},
		{"object":"script","description":"inline","sql":["select 1"]},
		{"object":"table","action":"drop","name":"old_table"}]}`
	if err := os.WriteFile(path, []byte(changeset), 0o644); err != nil {
		t.Fatal(err)
	}
	cs, err := ParseChangeSet(path)
	if err != nil {
		t.Fatalf("Error parsing changeset: %s", err)
	}
	fileDelta, ok := cs.ParsedDeltas[0].(SqlDelta)
	if !ok {
		t.Fatalf("Script file delta parsed as %T - EXPECTED: SqlDelta", cs.ParsedDeltas[0])
	}
	if fileDelta.Script != "backfill.sql" {
		t.Errorf("Script file name mismatch - EXPECTED: backfill.sql  ACTUAL: %s", fileDelta.Script)
	}
	if _, ok := cs.ParsedDeltas[1].(SqlDelta); !ok {
		t.Errorf("Inline sql delta parsed as %T - EXPECTED: SqlDelta", cs.ParsedDeltas[1])
	}
	if _, ok := cs.ParsedDeltas[2].(SimpleDelta); !ok {
		t.Errorf("Drop table delta parsed as %T - EXPECTED: SimpleDelta", cs.ParsedDeltas[2])
	}
}

func TestChangeSet_InTransaction(t *testing.T) {
	reader = osReadWrapper{}
	cases := map[string]bool{
		`{"description":"default","deltas":[]}`:                     true,
		`{"description":"explicit","transaction":true,"deltas":[]}`: true,
		`{"description":"opt out","transaction":false,"deltas":[]}`: false,
	}
	for changeset, expected := range cases {
		path := filepath.Join(t.TempDir(), "v1_0_1.json")
		if err := os.WriteFile(path, []byte(changeset), 0o644); err != nil {
			t.Fatal(err)
		}
		cs, err := ParseChangeSet(path)
		if err != nil {
			t.Fatalf("Error parsing changeset %s: %s", changeset, err)
		}
		if actual := cs.InTransaction(); actual != expected {
			t.Errorf("InTransaction mismatch for %s - EXPECTED: %t  ACTUAL: %t", changeset, expected, actual)
		}
	}
}

func TestListFiles(t *testing.T) {
	reader = osReadWrapper{}
	dir := t.TempDir()
	for _, name := range []string{"v1_0_1.json", "v1_0_2.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ListFiles(dir, "json")
	if err != nil || len(files) != 2 {
		t.Errorf("EXPECTED 2 json files  ACTUAL: %v (err %v)", files, err)
	}
	// a missing directory has no files
	files, err = ListFiles(filepath.Join(dir, "missing"), "json")
	if err != nil || len(files) != 0 {
		t.Errorf("missing directory - EXPECTED no files and no error  ACTUAL: %v (err %v)", files, err)
	}
	// any other read error is returned rather than treated as an empty directory
	if _, err = ListFiles(filepath.Join(dir, "notes.txt"), "json"); err == nil {
		t.Error("reading a file as a directory - EXPECTED an error  ACTUAL: nil")
	}
}
