package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"skima/pkg/drift"
	"skima/pkg/manifest"
	"skima/pkg/plan"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/viper"
)

// querier runs a query - a pooled connection or a transaction
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ReadSchemas reads the configured schema as it is in the database (actual) and as the state build describes it
// (expected). The expected schema comes from building the unit's steps into the session's temporary schema, in a
// transaction that is always rolled back - with pg_temp first on the search path, the state build's enums, sequences,
// tables, indexes, constraints, triggers and views are created as temporary objects, and postgres normalizes their definitions exactly
// as it did the real ones. Pass the state build without its post sql files, which load data. It only needs the TEMP
// privilege, takes no apply lock and changes nothing
func ReadSchemas(stateBuild plan.Unit) (actual drift.Schema, expected drift.Schema, err error) {
	conn := acquireConn()
	if conn == nil {
		return actual, expected, errors.New("unable to connect to the database")
	}
	defer conn.Release()
	ctx := context.Background()
	schema := viper.GetString("dbconn.schema")

	var namespace uint32
	var prefix string // the schema name as postgres prints it in index and trigger definitions
	err = conn.QueryRow(ctx, "select oid, quote_ident(nspname) from pg_namespace where nspname = $1", schema).Scan(&namespace, &prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return actual, expected, fmt.Errorf("schema %s does not exist", schema)
	} else if err != nil {
		return actual, expected, err
	}
	// read before the shadow build - once the temporary tables hide the real ones, view definitions name the schema
	// of the real tables they select from
	if actual, err = readSchema(ctx, conn, namespace, prefix); err != nil {
		return actual, expected, fmt.Errorf("unable to read schema %s: %w", schema, err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return actual, expected, err
	}
	defer tx.Rollback(ctx) // always - the shadow build is only read
	// lock_timeout guards against a pre sql file waiting on a lock held by the application
	_, err = tx.Exec(ctx, "select set_config('search_path', $1, true), set_config('lock_timeout', '10s', true)", "pg_temp, "+pgx.Identifier{schema}.Sanitize())
	if err != nil {
		return actual, expected, err
	}
	var failures strings.Builder
	shadow := executor{ex: savepointExecer{tx: tx, optOut: "it cannot be compared"}, record: noRecord, history: &ApplyHistory{}, out: io.Discard, errOut: &failures,
		shadow: true}
	if shadow.run(stateBuild.Steps) > 0 {
		// nothing is recorded, so the statement ids in the messages mean nothing
		messages := strings.ReplaceAll(failures.String(), " - Statement ID: 0", "")
		return actual, expected, fmt.Errorf("unable to build the schema the state files describe:\n%s", strings.TrimSpace(messages))
	}
	// a view that selects from a view or materialized view the build creates after it is bound to the real one when it
	// is created - the real schema is on the search path too, so it does not fail and wait for a retry. Now that every
	// temporary object exists, create the views again so they select from the build's objects
	for _, step := range stateBuild.Steps {
		var view string
		switch step.Kind {
		case plan.KindView:
			view = step.Sql[0]
		case plan.KindMatview:
			view = step.Shadow[0] // the scratch view with the materialized view's query
		default:
			continue
		}
		if _, err = tx.Exec(ctx, view); err != nil {
			return actual, expected, fmt.Errorf("unable to build the schema the state files describe: recreating view %s: %w", step.Describe, err)
		}
	}
	if err = tx.QueryRow(ctx, "select pg_my_temp_schema()").Scan(&namespace); err != nil {
		return actual, expected, err
	}
	if expected, err = readSchema(ctx, tx, namespace, "pg_temp"); err != nil {
		return actual, expected, fmt.Errorf("unable to read the schema the state files describe: %w", err)
	}
	for _, step := range stateBuild.Steps {
		if step.Kind == plan.KindMatview {
			standInMatview(expected, step.Describe)
		}
	}
	return actual, expected, nil
}

// standInMatview turns the objects that stood in for a materialized view in the shadow build back into the
// materialized view: its definition from the scratch view, its indexes and comment from the table with its name (see
// manifest.Matview.ShadowDdl)
func standInMatview(s drift.Schema, name string) {
	scratch := manifest.MatviewShadowPrefix + name
	mv := s.Matview(name)
	mv.Definition = s.Views[scratch].Definition
	if table, ok := s.Tables[name]; ok {
		mv.Indexes = table.Indexes
		mv.Comment = table.Comment
	}
	delete(s.Views, scratch)
	delete(s.Tables, name)
}

// noRecord records nothing - the shadow build is not an apply
func noRecord(history *ApplyHistory, stmtType string, status string, desc string) (int, error) {
	return 0, nil
}

// skima's own history tables are not part of the state files
const notHistoryTable = "c.relname not in ('skima_schema_history', 'skima_schema_statements')"

const tablesQuery = `select c.relname, coalesce(obj_description(c.oid, 'pg_class'), '') from pg_class c where c.relnamespace = $1 and c.relkind in ('r', 'p') and ` + notHistoryTable

const columnsQuery = `select c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
		coalesce(pg_get_expr(d.adbin, d.adrelid), ''),
		case a.attidentity when 'a' then 'always' when 'd' then 'by default' else '' end,
		case a.attgenerated when 's' then 'stored' when 'v' then 'virtual' else '' end,
		coalesce(col_description(a.attrelid, a.attnum), '')
	from pg_attribute a
	join pg_class c on c.oid = a.attrelid
	left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
	where c.relnamespace = $1 and c.relkind in ('r', 'p') and a.attnum > 0 and not a.attisdropped and ` + notHistoryTable

// check, foreign key, primary key, unique and exclusion constraints - not null is compared on the column
const constraintsQuery = `select c.relname, con.conname, pg_get_constraintdef(con.oid)
	from pg_constraint con join pg_class c on c.oid = con.conrelid
	where c.relnamespace = $1 and con.contype in ('c', 'f', 'p', 'u', 'x') and ` + notHistoryTable

// indexes of tables and materialized views - those that back a primary key, unique or exclusion constraint are
// compared as the constraint
const indexesQuery = `select c.relname, c.relkind = 'm', i.relname, pg_get_indexdef(x.indexrelid)
	from pg_index x join pg_class i on i.oid = x.indexrelid join pg_class c on c.oid = x.indrelid
	where c.relnamespace = $1 and ` + notHistoryTable + ` and not exists (
		select 1 from pg_constraint con where con.conindid = x.indexrelid and con.contype in ('p', 'u', 'x'))`

const triggersQuery = `select c.relname, t.tgname, pg_get_triggerdef(t.oid)
	from pg_trigger t join pg_class c on c.oid = t.tgrelid
	where c.relnamespace = $1 and not t.tgisinternal and ` + notHistoryTable

const enumsQuery = `select t.typname, array_agg(e.enumlabel order by e.enumsortorder), coalesce(obj_description(t.oid, 'pg_type'), '')
	from pg_type t join pg_enum e on e.enumtypid = t.oid
	where t.typnamespace = $1
	group by t.oid, t.typname`

// sequences of identity columns (an 'i' dependency on their column) are compared as the column. The owner is the
// column of an 'a' (auto) dependency - set by owned by, or by a serial column
const sequencesQuery = `select c.relname, format_type(s.seqtypid, null), s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle,
		coalesce((select t.relname || '.' || a.attname from pg_depend d
			join pg_class t on t.oid = d.refobjid
			join pg_attribute a on a.attrelid = d.refobjid and a.attnum = d.refobjsubid
			where d.classid = 'pg_class'::regclass and d.objid = c.oid and d.deptype = 'a' and d.refobjsubid > 0), ''),
		coalesce(obj_description(c.oid, 'pg_class'), '')
	from pg_class c join pg_sequence s on s.seqrelid = c.oid
	where c.relnamespace = $1 and c.relkind = 'S' and not exists (
		select 1 from pg_depend d where d.classid = 'pg_class'::regclass and d.objid = c.oid and d.deptype = 'i')`

const matviewsQuery = `select c.relname, pg_get_viewdef(c.oid), coalesce(obj_description(c.oid, 'pg_class'), '')
	from pg_class c where c.relnamespace = $1 and c.relkind = 'm'`

const viewsQuery = `select c.relname, pg_get_viewdef(c.oid), coalesce(obj_description(c.oid, 'pg_class'), '') from pg_class c where c.relnamespace = $1 and c.relkind = 'v'`

// readSchema reads the tables and views in the namespace. prefix is the schema name as postgres prints it in index and
// trigger definitions - it is removed so definitions from different schemas compare equal
func readSchema(ctx context.Context, q querier, namespace uint32, prefix string) (drift.Schema, error) {
	s := drift.NewSchema()
	unqualify := func(definition string) string {
		return strings.NewReplacer(" ON "+prefix+".", " ON ", " ON ONLY "+prefix+".", " ON ONLY ").Replace(definition)
	}
	readings := []struct {
		query string
		scan  func(rows pgx.Rows) error
	}{
		{tablesQuery, func(rows pgx.Rows) error {
			var table, comment string
			err := rows.Scan(&table, &comment)
			s.Table(table).Comment = comment
			return err
		}},
		{columnsQuery, func(rows pgx.Rows) error {
			var table, name string
			var col drift.Column
			err := rows.Scan(&table, &name, &col.Type, &col.NotNull, &col.Default, &col.Identity, &col.Generated, &col.Comment)
			s.Table(table).Columns[name] = col
			return err
		}},
		{constraintsQuery, func(rows pgx.Rows) error {
			var table, name, def string
			err := rows.Scan(&table, &name, &def)
			s.Table(table).Constraints[name] = def
			return err
		}},
		{indexesQuery, func(rows pgx.Rows) error {
			var table, name, def string
			var matview bool
			err := rows.Scan(&table, &matview, &name, &def)
			if matview {
				s.Matview(table).Indexes[name] = unqualify(def)
			} else {
				s.Table(table).Indexes[name] = unqualify(def)
			}
			return err
		}},
		{matviewsQuery, func(rows pgx.Rows) error {
			var name, def, comment string
			err := rows.Scan(&name, &def, &comment)
			mv := s.Matview(name)
			mv.Definition, mv.Comment = def, comment
			return err
		}},
		{triggersQuery, func(rows pgx.Rows) error {
			var table, name, def string
			err := rows.Scan(&table, &name, &def)
			s.Table(table).Triggers[name] = unqualify(def)
			return err
		}},
		{enumsQuery, func(rows pgx.Rows) error {
			var name string
			var enum drift.Enum
			err := rows.Scan(&name, &enum.Values, &enum.Comment)
			s.Enums[name] = enum
			return err
		}},
		{sequencesQuery, func(rows pgx.Rows) error {
			var name string
			var seq drift.Sequence
			err := rows.Scan(&name, &seq.Type, &seq.Start, &seq.Increment, &seq.Min, &seq.Max, &seq.Cache, &seq.Cycle, &seq.OwnedBy, &seq.Comment)
			s.Sequences[name] = seq
			return err
		}},
		{viewsQuery, func(rows pgx.Rows) error {
			var name string
			var view drift.View
			err := rows.Scan(&name, &view.Definition, &view.Comment)
			s.Views[name] = view
			return err
		}},
	}
	for _, reading := range readings {
		rows, err := q.Query(ctx, reading.query, namespace)
		if err != nil {
			return s, err
		}
		for rows.Next() {
			if err = reading.scan(rows); err != nil {
				rows.Close()
				return s, err
			}
		}
		if err = rows.Err(); err != nil {
			return s, err
		}
	}
	return s, nil
}
