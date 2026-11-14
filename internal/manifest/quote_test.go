package manifest

import "testing"

func TestQuoteIdent(t *testing.T) {
	for name, want := range map[string]string{
		"documents":   "documents",
		"Documents":   "Documents", // folded to lower case unquoted, as skima always created it
		"_private$1":  "_private$1",
		"user":        `"user"`,
		"Order":       `"Order"`,
		"my table":    `"my table"`,
		"2fa":         `"2fa"`,
		`say"hi"`:     `"say""hi"""`,
		"label_order": "label_order",
	} {
		if got := QuoteIdent(name); got != want {
			t.Errorf("QuoteIdent(%q) - EXPECTED %s  ACTUAL: %s", name, want, got)
		}
	}
	if got := quoteQualified("user.order"); got != `"user"."order"` {
		t.Errorf("quoteQualified - ACTUAL: %s", got)
	}
	// a list entry is quoted only when it is a plain name - expressions and several columns in one entry are used as written
	if got := quoteList([]string{"id", "user", "lower(email)", "label desc", "a, b"}, ", "); got != `id, "user", lower(email), label desc, a, b` {
		t.Errorf("quoteList - ACTUAL: %s", got)
	}
}

// generated DDL for a table named with keywords
func TestKeywordNamesInDdl(t *testing.T) {
	table := Table{Name: "user", PrimaryKey: []string{"id"},
		Columns:     []Column{{Name: "id", Datatype: "integer"}, {Name: "order", Datatype: "text", Nullable: true, Default: "it's"}},
		Indexes:     []Index{{Name: "user_order_idx", Columns: []string{"order"}}},
		ForeignKeys: []ForeignKey{{Name: "user_group_fk", Columns: []string{"group"}, Reference: PrimaryKeyRef{Table: "group", Columns: []string{"id"}}}},
		Constraints: []Constraint{{Name: "user_order_unq", Type: "unique", Columns: []string{"order"}}}}
	for _, tc := range []struct{ got, want string }{
		{table.GenerateDdl(), `create table if not exists "user" (id integer not null,"order" text default 'it''s' , constraint user_pk primary key (id)) `},
		{table.Indexes[0].GenerateDdl("user"), `create  index if not exists user_order_idx on "user" ("order")`},
		{table.ForeignKeys[0].AlterStmt("user", "add"), `alter table "user" add constraint user_group_fk foreign key ("group") references "group" (id)`},
		{table.Constraints[0].GenerateDdl("user"), `alter table "user" add constraint user_order_unq unique ("order")`},
		{table.Columns[1].AlterStmt("user", "drop"), `alter table "user" drop column if exists "order"`},
		{CommentDdl("column", "user.order", "x"), `comment on column "user"."order" is 'x'`},
	} {
		if tc.got != tc.want {
			t.Errorf("EXPECTED %s\n  ACTUAL:  %s", tc.want, tc.got)
		}
	}
}
