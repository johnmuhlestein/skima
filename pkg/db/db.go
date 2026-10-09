package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"skima/pkg/manifest"
	"skima/pkg/util"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/viper"
)

const histInsert = "insert into skima_schema_history (schema_version, apply_type, status, delta_file) values ($1,$2,$3,$4) returning run_id"
const histUpdateComp = "update skima_schema_history set status = $1, completion_timestamp = now() where run_id = $2"
const histLatest = "select run_id, schema_version, apply_start_timestamp, completion_timestamp, apply_type, status, coalesce(delta_file, '') from skima_schema_history where run_id = (select max(run_id) from skima_schema_history where status in ('complete','success'))"
const histStatements = "select run_id, stmt_id, stmt_type, status, coalesce(description,'') from skima_schema_statements where run_id = $1 order by stmt_id"

const histStmtRolledBack = "update skima_schema_statements set status = 'rolled back' where run_id = $1 and status = 'success'"

const histStmtInsert = "insert into skima_schema_statements (run_id, stmt_type, status, description) values ($1,$2,$3,$4) returning stmt_id"

const bootstrapUser = "create user %s with password '%s'"
const bootstrapSchema = "create schema if not exists %s authorization %s"
const bootstrapGrant = "grant all privileges on schema %s to %s with grant option"
const bootstrapSearchPath = "alter role %s set search_path to %s"

const histTableName = "skima_schema_history"

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

var histApplyTypes = util.FromSlice([]string{"state", "baseline", "delta", "changeset", "sql"})
var once sync.Once
var instance *PgPool
var NoHistTable error = errors.New("history tables have not been provisioned")

type PgPool struct {
	pool *pgxpool.Pool
}

type Metadata struct {
	Version       string
	LastApplyDate time.Time
	Status        string
}

type ApplyHistory struct {
	RunId               int
	SchemaVersion       util.SemanticVersion `json:"-"`
	VersionString       string
	ApplyTimestamp      time.Time
	CompletionTimestamp time.Time
	ApplyType           string
	Status              string
	FileName            string
	Statements          []ApplyStatement
}

type ApplyStatement struct {
	StatementId   int
	StatementType string
	Status        string
	Description   string
}

func Connect() *PgPool {
	once.Do(func() {
		poolConfig, err := pgxpool.ParseConfig(connString(viper.GetString("dbconn.username"), viper.GetString("dbconn.password")))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to parse database connection settings:", err)
			os.Exit(1)
		}
		// set as a runtime param rather than a URL "options" query value, which newer pgx versions reject
		poolConfig.ConnConfig.RuntimeParams["search_path"] = viper.GetString("dbconn.schema")
		newPool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to connect to database:", err)
			os.Exit(1)
		}
		instance = &PgPool{pool: newPool}
		migrateLegacyHistTables(newPool)
	})
	return instance
}

// renames the history tables (and their sequences/constraints) created when this tool was named perseus - runs as
// a single statement so the rename either fully completes or not at all
const legacyHistRename = `do $$
begin
  alter table perseus_schema_history rename to skima_schema_history;
  if to_regclass('perseus_schema_history_run_id_seq') is not null then
    alter sequence perseus_schema_history_run_id_seq rename to skima_schema_history_run_id_seq;
  end if;
  if to_regclass('perseus_schema_history_pkey') is not null then
    alter index perseus_schema_history_pkey rename to skima_schema_history_pkey;
  end if;
  if to_regclass('perseus_schema_statements') is not null then
    alter table perseus_schema_statements rename to skima_schema_statements;
    if to_regclass('perseus_schema_statements_stmt_id_seq') is not null then
      alter sequence perseus_schema_statements_stmt_id_seq rename to skima_schema_statements_stmt_id_seq;
    end if;
    if to_regclass('perseus_schema_statements_pkey') is not null then
      alter index perseus_schema_statements_pkey rename to skima_schema_statements_pkey;
    end if;
    if exists (select 1 from pg_constraint where conname = 'perseus_hist_fk' and conrelid = 'skima_schema_statements'::regclass) then
      alter table skima_schema_statements rename constraint perseus_hist_fk to skima_hist_fk;
    end if;
  end if;
end $$`

// migrateLegacyHistTables renames perseus_* history tables to skima_* so that schemas managed before the rename are
// still recognized - otherwise they would look unmanaged and have their state re-applied
func migrateLegacyHistTables(pool *pgxpool.Pool) {
	var legacy, current bool
	err := pool.QueryRow(context.Background(), "select to_regclass('perseus_schema_history') is not null, to_regclass('skima_schema_history') is not null").Scan(&legacy, &current)
	if checkErr(err, "Unable to check for legacy perseus history tables:") {
		os.Exit(1)
	}
	switch {
	case legacy && current:
		fmt.Fprintln(os.Stderr, "WARNING: both perseus_schema_history and skima_schema_history exist - using skima_schema_history and leaving the perseus tables untouched")
	case legacy:
		_, err = pool.Exec(context.Background(), legacyHistRename)
		if checkErr(err, "Unable to rename the legacy perseus history tables to skima:") {
			os.Exit(1)
		}
		fmt.Println("Renamed legacy perseus history tables to skima_schema_history and skima_schema_statements")
	}
}

// role and schema names accepted by bootstrap - postgres' unquoted identifier rules, so the names behave the same whether
// or not they are quoted elsewhere (e.g. the search_path)
var identifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_$]{0,62}$`)

// connString builds a connection URL for the configured host, port and database. The credentials are URL-escaped so
// they may contain any characters
func connString(user string, password string) string {
	connUrl := url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(viper.GetString("dbconn.connstring"), strconv.Itoa(viper.GetInt("dbconn.port"))),
		Path:   "/" + viper.GetString("dbconn.database"),
	}
	return connUrl.String()
}

// asPgError returns the error reported by the postgres server, or nil when err is nil or did not come from the
// server (e.g. a dropped connection or network failure)
func asPgError(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}

func checkErr(err error, msg string) bool {
	if err != nil {
		fmt.Fprintln(os.Stderr, msg, err)
		return true
	}
	return false
}

// execer runs a statement - either a pooled connection or a changeset's transaction
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// savepointExecer runs each statement of a transaction in its own savepoint. An error aborts a postgres transaction,
// but some errors are tolerated (e.g. "already exists", or a sql delta's bypass codes) - rolling back to the savepoint
// undoes just the failed statement so the rest of the transaction can continue
type savepointExecer struct {
	tx pgx.Tx
}

func (se savepointExecer) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	savepoint, err := se.tx.Begin(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := savepoint.Exec(ctx, sql, arguments...)
	if err != nil {
		if pgErr := asPgError(err); pgErr != nil && pgErr.Code == "25001" { // active_sql_transaction
			fmt.Fprintln(os.Stderr, `  this statement cannot run inside a transaction - set "transaction": false on the changeset to apply it without one`)
		}
		savepoint.Rollback(ctx)
		return tag, err
	}
	return tag, savepoint.Commit(ctx)
}

func acquireConn() *pgxpool.Conn {
	if instance == nil {
		Connect()
	}
	conn, err := instance.pool.Acquire(context.Background())
	if checkErr(err, "Error acquiring connection for user") {
		return nil
	}
	return conn
}

// ApplyChangeset applies every delta in the changeset, returning the number of failures. Unless the changeset opts out
// with "transaction": false, it is applied in a single transaction - any failure rolls back the whole changeset so the
// schema is left at the previous version. The history is written on separate connections so it survives a rollback.
func ApplyChangeset(history *ApplyHistory, cs manifest.ChangeSet) int {
	conn := acquireConn()
	if conn == nil {
		history.Status = "failed"
		return 1
	}
	defer conn.Release()
	if !cs.InTransaction() {
		fmt.Printf("Changeset %s is applied without a transaction - a failure part way through is not rolled back\n", cs.Version)
		return applyDeltas(conn, history, cs)
	}

	tx, err := conn.Begin(context.Background())
	if checkErr(err, "Unable to start the changeset transaction:") {
		history.Status = "failed"
		return 1
	}
	defer tx.Rollback(context.Background()) // no-op once committed
	failures := applyDeltas(savepointExecer{tx: tx}, history, cs)
	if failures == 0 {
		// deferred constraints are checked at commit, so a commit can still fail
		if err = tx.Commit(context.Background()); err != nil {
			stmtId, err2 := recordStatementHist(history, "Commit changeset", "failed", err.Error())
			fmt.Fprintf(os.Stderr, "FAILURE: committing changeset %s - Statement ID: %d\n %s\n", cs.Version, stmtId, err)
			checkErr(err2, "Unable to record the statement history for committing the changeset")
			failures++
		} else {
			return 0
		}
	}
	if err = tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		if conn.Conn().IsClosed() {
			fmt.Fprintln(os.Stderr, "The database connection was lost - the server discards the open changeset transaction")
		} else {
			fmt.Fprintln(os.Stderr, "Unable to roll back the changeset transaction:", err)
		}
	}
	markRolledBack(history)
	fmt.Fprintf(os.Stderr, "Changeset %s was rolled back - none of its changes were kept\n", cs.Version)
	return failures
}

// markRolledBack records that a run's changes were undone - statements recorded as successful did not persist
func markRolledBack(history *ApplyHistory) {
	history.Status = "rolled back"
	conn := acquireConn()
	if conn == nil {
		return
	}
	defer conn.Release()
	_, err := conn.Exec(context.Background(), histStmtRolledBack, history.RunId)
	checkErr(err, "Unable to mark the run's statements as rolled back:")
}

// LockApply takes a postgres advisory lock for the schema so that only one apply runs against it at a time - two
// deploy jobs would otherwise apply the same changesets concurrently. If another apply holds the lock, this waits for
// it to finish. The lock is held on a dedicated connection until the process exits.
func LockApply() {
	conn := acquireConn()
	if conn == nil {
		os.Exit(1)
	}
	key := "skima apply " + viper.GetString("dbconn.schema")
	var locked bool
	err := conn.QueryRow(context.Background(), "select pg_try_advisory_lock(hashtext($1))", key).Scan(&locked)
	if checkErr(err, "Unable to take the apply lock:") {
		os.Exit(1)
	}
	if !locked {
		fmt.Printf("Another skima apply is running against schema %s - waiting for it to finish\n", viper.GetString("dbconn.schema"))
		_, err = conn.Exec(context.Background(), "select pg_advisory_lock(hashtext($1))", key)
		if checkErr(err, "Unable to take the apply lock:") {
			os.Exit(1)
		}
	}
	// the connection is intentionally never released - closing it at exit releases the lock
}

func applyDeltas(ex execer, history *ApplyHistory, cs manifest.ChangeSet) int {
	var failures int
	fks := make([]manifest.Table, 0)
	for _, delta := range cs.ParsedDeltas {
		deltaSkipped := false
		if !prehook(ex, history, delta) {
			failures++
			return failures
		}
		sd, k := delta.(manifest.SimpleDelta)
		qd, q := delta.(manifest.SqlDelta)
		if k && sd.Action == "add" && sd.Object == "table" {
			table := sd.Table()
			ok := applyTable(ex, history, table)
			if !ok {
				failures++
				history.Status = "fast-fail"
				return failures
			}
			if len(table.ForeignKeys) > 0 {
				fks = append(fks, table)
			}
		} else if q && len(qd.Script) < 1 {
			ok := applySql(ex, history, "delta", qd.Sql, qd.Bypass)
			if !ok {
				failures++
				return failures
			}
		} else if q {
			// a script file delta - a missing or empty script is a failure, not a skipped delta
			script := qd.GenerateDdl()
			if len(script) < 1 {
				stmtId, err2 := recordStatementHist(history, qd.StatementType(), "failed", fmt.Sprintf("script %s is missing or empty", qd.Script))
				fmt.Fprintf(os.Stderr, "FAILURE: delta script %s is missing or empty - Statement ID: %d\n", qd.Script, stmtId)
				checkErr(err2, "Unable to record the statement history for applying the delta")
				history.Status = "fast-fail"
				failures++
				return failures
			}
			if !applySql(ex, history, "delta script", []string{script}, qd.Bypass) {
				failures++
				return failures
			}
		} else {
			var ok bool
			ddl := delta.GenerateDdl()
			if len(ddl) < 1 {
				stmtId, err2 := recordStatementHist(history, delta.StatementType(), "deltaSkipped", "No DDL Generated")
				fmt.Fprintf(os.Stderr, "WARNING: applying %s was deltaSkipped due to no DDL this could be due to a downstream change that removed this reference object from state definition - Statement ID: %d\n", delta.Describe(), stmtId)
				checkErr(err2, "Unable to record the statement history for applying the delta")
				deltaSkipped = true
			} else {
				_, err := ex.Exec(context.Background(), ddl)
				pgErr := asPgError(err)
				switch {
				case err == nil:
					ok = true
				case pgErr != nil:
					switch pgErr.Code {
					case "42P07": // unique constraint already exists
						ok = true
					case "42710": // check constraint already exists
						ok = true
					case "42P01": // view does not exist
						ok = true
					case "42703": // column does not exist
						if strings.Contains(delta.StatementType(), "rename") {
							ok = true
						} else {
							ok = false
						}
					case "42704": // fk does not exist
						if strings.Contains(delta.StatementType(), "drop") {
							ok = true
						} else {
							ok = false
						}
					default:
						ok = false
					}
				default:
					ok = false
				}

				if !ok {
					failures++
					stmtId, err2 := recordStatementHist(history, delta.StatementType(), "failed", err.Error())
					history.Status = "fast-fail"
					fmt.Println(delta.GenerateDdl())
					fmt.Fprintf(os.Stderr, "FAILURE: applying %s failed - Statement ID: %d\n %s\n", delta.Describe(), stmtId, err)
					checkErr(err2, "Unable to record the statement history for applying the delta")
					return failures
				} else {
					stmtId, err2 := recordStatementHist(history, delta.StatementType(), "success", delta.Describe())
					fmt.Printf("SUCCESS: %s applied to the database - Statement ID: %d\n", delta.Describe(), stmtId)
					checkErr(err2, "Unable to record the statement history for applying the delta")
				}
			}
		}
		if !deltaSkipped && !posthook(ex, history, delta) {
			failures++
			return failures
		}
	}

	// Now apply the foreign keys - we do this at the end to make sure all tables have been added
	for _, tbl := range fks {
		ok := applyForeignKeys(ex, history, tbl)
		if !ok {
			failures++
			history.Status = "fast-fail"
			return failures
		}
	}
	if failures > 0 {
		history.Status = "failed"
	}
	return failures
}

// hooked is implemented by the deltas that support pre/post hooks - those that embed manifest.PrePostHook
type hooked interface {
	Hooks() manifest.PrePostHook
}

// prehook executes the delta's pre hook, if it has one. It returns false if the hook failed (the history is already
// written) and true if there is no hook or it succeeded
func prehook(ex execer, history *ApplyHistory, delta manifest.Delta) bool {
	if d, ok := delta.(hooked); ok {
		return runHook(ex, history, d.Hooks().Pre, "pre-hook")
	}
	return true
}

// posthook executes the delta's post hook, if it has one. It returns false if the hook failed (the history is already
// written) and true if there is no hook or it succeeded
func posthook(ex execer, history *ApplyHistory, delta manifest.Delta) bool {
	if d, ok := delta.(hooked); ok {
		return runHook(ex, history, d.Hooks().Post, "post-hook")
	}
	return true
}

// runHook executes a hook's sql statements, or its script file when it has no sql
func runHook(ex execer, history *ApplyHistory, hook manifest.Hook, hookType string) bool {
	var ok bool
	switch {
	case len(hook.Sql) > 0:
		ok = applySql(ex, history, hookType, hook.Sql, []string{})
	case len(hook.Script) > 0:
		ok = applyScriptHook(ex, history, hook.Script)
	default:
		return true
	}
	if !ok {
		history.Status = "fast-fail"
	}
	return ok
}

func applySql(ex execer, history *ApplyHistory, sqlType string, statements []string, acceptableErrors []string) bool {
	errSet := util.FromSlice(acceptableErrors)

	for idx, statement := range statements {
		_, err := ex.Exec(context.Background(), statement)
		if err != nil {
			if pgErr := asPgError(err); pgErr != nil {
				code := pgErr.Code
				message := pgErr.Message
				if errSet.Has(code) {
					stmtId, err2 := recordStatementHist(history, fmt.Sprintf("%s sql [%d]", sqlType, idx), "success", fmt.Sprintf("{[%s] %s} - %s", code, message, statement))
					fmt.Printf("SUCCESS: sql %s at index [%d] marked as success because failure was due to acceptable condition [%s] %s  Statement ID: %d\n", sqlType, idx, code, message, stmtId)
					checkErr(err2, "Unable to record the statement history for 'SQL statement'")
				} else {
					stmtId, err2 := recordStatementHist(history, fmt.Sprintf("%s sql [%d]", sqlType, idx), "failed", fmt.Sprintf("{[%s] %s} - %s", code, message, statement))
					fmt.Printf("FAILURE: sql %s at index [%d] marked as failed because failure was due to unacceptable condition [%s] %s  Statement ID: %d\n", sqlType, idx, code, message, stmtId)
					checkErr(err2, "Unable to record the statement history for 'SQL statement'")
					history.Status = "failed"
					return false
				}
			} else {
				stmtId, err2 := recordStatementHist(history, fmt.Sprintf("%s sql [%d]", sqlType, idx), "failed", err.Error())
				fmt.Fprintf(os.Stderr, "FAILURE: executing sql based %s; statement index [%d] - Statement ID: %d\n%s\n", sqlType, idx, stmtId, err)
				checkErr(err2, "Unable to record the statement history for 'SQL statement'")
				return false
			}
		} else {
			stmtId, err2 := recordStatementHist(history, fmt.Sprintf("%s sql [%d]", sqlType, idx), "success", statement)
			fmt.Printf("SUCCESS: sql %s at index [%d] Statement ID: %d\n", sqlType, idx, stmtId)
			checkErr(err2, "Unable to record the statement history for 'SQL statement'")
		}
	}
	return true
}

func applyScriptHook(ex execer, history *ApplyHistory, script string) bool {
	// same location as script deltas (see manifest.SqlDelta.GenerateDdl)
	path := filepath.Join(viper.GetString("workdir"), "deltas", "scripts", script)
	scriptBytes, err := reader.readFile(path)
	if err != nil {
		stmtId, err2 := recordStatementHist(history, fmt.Sprintf("Delta script hook [%s]", script), "failed", err.Error())
		fmt.Fprintf(os.Stderr, "FAILURE: Unable to read delta hook file %s Statement ID: %d\n%s\n", path, stmtId, err)
		checkErr(err2, "Unable to record the statement history for 'Delta sql hook'")
		return false
	}
	_, err = ex.Exec(context.Background(), string(scriptBytes[:]))
	if err != nil {
		stmtId, err2 := recordStatementHist(history, fmt.Sprintf("Delta script hook [%s]", script), "failed", err.Error())
		fmt.Fprintf(os.Stderr, "FAILURE: executing script based hook; %s - Statement ID: %d\n%s\n", script, stmtId, err)
		checkErr(err2, "Unable to record the statement history for 'Delta script hook'")
		return false
	}
	stmtId, err2 := recordStatementHist(history, "Delta script hook", "success", script)
	fmt.Printf("SUCCESS: script hook %s Statement ID: %d\n", script, stmtId)
	checkErr(err2, "Unable to record the statement history for 'Delta script hook'")
	return true
}

// ApplyTable creates the table with its indexes, triggers and constraints on a pooled connection
func ApplyTable(history *ApplyHistory, tbl manifest.Table) bool {
	conn := acquireConn()
	if conn == nil {
		return false
	}
	defer conn.Release()
	return applyTable(conn, history, tbl)
}

func applyTable(ex execer, history *ApplyHistory, tbl manifest.Table) bool {
	ok := false
	status := "failed"
	_, err := ex.Exec(context.Background(), tbl.GenerateDdl())
	if err != nil {
		stmtId, err2 := recordStatementHist(history, "Create Table", status, err.Error())
		fmt.Println(tbl.GenerateDdl())
		fmt.Fprintf(os.Stderr, "FAILURE: adding table %s to the database - Statement ID: %d\n %s\n", tbl.Name, stmtId, err)
		checkErr(err2, "Unable to record the statement history for applying the table")
	} else {
		ok = true
		status = "success"
		fmt.Printf("SUCCESS: Table %s base structure complete - adding indexes\n", tbl.Name)
		for _, idx := range tbl.Indexes {
			_, err = ex.Exec(context.Background(), idx.GenerateDdl(tbl.Name))
			if err != nil {
				ok = false
				status = "failed"
				fmt.Println("  ", idx.GenerateDdl(tbl.Name))
				fmt.Fprintf(os.Stderr, "  FAILURE: Index %s failed:\n    %s\n", idx.Name, err)
			} else {
				fmt.Printf("  SUCCESS: Index %s was added to %s\n", idx.Name, tbl.Name)
			}
		}
		for _, trg := range tbl.Triggers {
			_, err = ex.Exec(context.Background(), trg.Ddl)
			pgErr := asPgError(err)
			// tracked per trigger so a success never hides an earlier failure on this table
			trgOk := err == nil || (pgErr != nil && pgErr.Code == "42710") // 42710: trigger already exists
			if !trgOk {
				ok = false
				status = "failed"
				fmt.Println("  ", trg.Ddl)
				fmt.Fprintf(os.Stderr, "  FAILURE: Trigger %s failed:\n    %s\n", trg.Name, err)
			} else {
				fmt.Printf("  SUCCESS: Trigger %s was added to %s\n", trg.Name, tbl.Name)
			}
		}
		for _, cons := range tbl.Constraints {
			_, err = ex.Exec(context.Background(), cons.GenerateDdl(tbl.Name))
			pgErr := asPgError(err)
			// 42P07: unique constraint already exists, 42710: check constraint already exists
			consOk := err == nil || (pgErr != nil && (pgErr.Code == "42P07" || pgErr.Code == "42710"))
			if !consOk {
				ok = false
				status = "failed"
				fmt.Println("  ", cons.GenerateDdl(tbl.Name))
				fmt.Fprintf(os.Stderr, "  FAILURE: Constraint %s failed:\n    %s\n", cons.Name, err)
			} else {
				fmt.Printf("  SUCCESS: Constraint %s was added to %s\n", cons.Name, tbl.Name)
			}
		}
		stmtId, err2 := recordStatementHist(history, "Create Table", status, tbl.Name)
		fmt.Printf("%s: Table %s was added to the database - Statement ID: %d\n", strings.ToUpper(status), tbl.Name, stmtId)
		checkErr(err2, "Unable to record the statement history for applying the table")
	}
	return ok
}

// ApplyView creates the view. When it fails, retryable reports whether the failure was a missing relation - the view
// may depend on another view that has not been created yet, so it is worth trying again once the others are applied
func ApplyView(history *ApplyHistory, view manifest.View) (ok bool, retryable bool) {
	status := "failed"
	if instance == nil {
		Connect()
	}
	conn, err := instance.pool.Acquire(context.Background())
	if checkErr(err, "Error acquiring connection for user") {
		return false, false
	}
	defer conn.Release()
	_, err = conn.Exec(context.Background(), view.GenerateDdl())
	var description string
	ok = err == nil
	if pgErr := asPgError(err); pgErr != nil && pgErr.Code == "42P01" { // relation does not exist
		retryable = true
		fmt.Fprintf(os.Stderr, "  view %s references a relation that does not exist yet: %s\n", view.Name, pgErr.Message)
	}
	if !ok {
		description = err.Error()
		fmt.Println("View DDL:", view.GenerateDdl())
	} else {
		status = "success"
		description = view.Name
	}
	stmtId, err2 := recordStatementHist(history, "Create View", status, description)
	fmt.Printf("%s: adding view %s to the database - Statement ID: %d\n", strings.ToUpper(status), view.Name, stmtId)
	checkErr(err2, "Unable to record the statement history for applying the view")
	return ok, retryable
}

// ApplyForeignKeys adds every foreign key on the table on a pooled connection, returning false if any of them failed
func ApplyForeignKeys(history *ApplyHistory, tbl manifest.Table) bool {
	conn := acquireConn()
	if conn == nil {
		return false
	}
	defer conn.Release()
	return applyForeignKeys(conn, history, tbl)
}

func applyForeignKeys(ex execer, history *ApplyHistory, tbl manifest.Table) bool {
	ok := true
	stmtType := "Add FK"
	for _, fk := range tbl.ForeignKeys {
		_, err := ex.Exec(context.Background(), fk.AlterStmt(tbl.Name, "add"))
		pgErr := asPgError(err)
		switch {
		case err == nil:
			stmtId, err2 := recordStatementHist(history, stmtType, "success", fmt.Sprintf("%s.%s", tbl.Name, fk.Name))
			fmt.Printf("SUCCESS: FK %s.%s added to the database - Statement ID: %d\n", tbl.Name, fk.Name, stmtId)
			checkErr(err2, "Unable to record the statement history for applying the FK")
		case pgErr != nil:
			switch pgErr.Code {
			case "42710": // already exists
				stmtId, err2 := recordStatementHist(history, stmtType, "success", fmt.Sprintf("%s.%s", tbl.Name, fk.Name))
				fmt.Printf("SUCCESS: FK %s.%s already exists - Statement ID: %d\n", tbl.Name, fk.Name, stmtId)
				checkErr(err2, "Unable to record the statement history for applying the FK")
			default:
				ok = false
				stmtId, err2 := recordStatementHist(history, stmtType, "failed", pgErr.Message)
				fmt.Println(fk.AlterStmt(tbl.Name, "add"))
				fmt.Fprintf(os.Stderr, "FAILURE: adding FK %s.%s to the database - Statement ID: %d\n -- %s", tbl.Name, fk.Name, stmtId, err)
				checkErr(err2, "Unable to record the statement history for applying the FK")
			}
		default:
			ok = false
			stmtId, err2 := recordStatementHist(history, stmtType, "failed", err.Error())
			fmt.Println(fk.AlterStmt(tbl.Name, "add"))
			fmt.Fprintf(os.Stderr, "FAILURE: adding FK %s.%s to the database - Statement ID: %d\n -- %s", tbl.Name, fk.Name, stmtId, err)
			checkErr(err2, "Unable to record the statement history for applying the FK")
		}
	}
	return ok
}

func ApplySql(history *ApplyHistory, filename string, sql string) bool {
	ok := false
	if instance == nil {
		Connect()
	}
	conn, err := instance.pool.Acquire(context.Background())
	if checkErr(err, "Error acquiring connection for user") {
		return false
	}
	defer conn.Release()
	_, err = conn.Exec(context.Background(), sql)
	if err != nil {
		ok = false
		history.Status = "failed"
		stmtId, err2 := recordStatementHist(history, "sql", "failed", err.Error())
		fmt.Fprintf(os.Stderr, "FAILURE: adding SQL %s to the database - Statement ID: %d\n -- %s\n", filename, stmtId, err)
		checkErr(err2, "Unable to record the statement history")
	} else {
		ok = true
		stmtId, err2 := recordStatementHist(history, "sql", "success", filename)
		fmt.Printf("SUCCESS: added SQL %s to the database - Statement ID %d\n", filename, stmtId)
		checkErr(err2, "Unable to record the statement history")
	}
	return ok
}

// InitializeHistory Creates a new history record in the database, which generates a runId and populates the basic
// information. The filename parameter is optional, and passing in an empty string will leave the field blank in
// the database
func InitializeHistory(version util.SemanticVersion, applyType string, filename string) ApplyHistory {
	histStatus := "started"
	ok := histApplyTypes.Has(applyType)
	hist := ApplyHistory{SchemaVersion: version, ApplyType: applyType, Status: histStatus, VersionString: version.String(), FileName: filename}
	if !ok {
		fmt.Fprintln(os.Stderr, "Invalid apply type for the history table:", applyType)
		os.Exit(1)
	}
	if applyType == "baseline" {
		hist.Status = "complete"
	}
	if instance == nil {
		Connect()
	}
	conn, err := instance.pool.Acquire(context.Background())
	if checkErr(err, "Error acquiring connection for user") {
		os.Exit(1)
	}
	defer conn.Release()
	err = conn.QueryRow(context.Background(), histInsert, hist.SchemaVersion.String(), hist.ApplyType, hist.Status, hist.FileName).Scan(&hist.RunId)
	if checkErr(err, fmt.Sprintf("Error initializing the run: %s", err)) {
		os.Exit(1)
	}
	return hist
}

func FinalizeHistory(history *ApplyHistory) {
	if instance == nil {
		Connect()
	}
	conn, err := instance.pool.Acquire(context.Background())
	if checkErr(err, "Error acquiring connection for user") {
		os.Exit(1)
	}
	defer conn.Release()
	_, err = conn.Exec(context.Background(), histUpdateComp, history.Status, history.RunId)
	if err != nil {
		fmt.Fprint(os.Stderr, "Unable to finalize the history record: ", err)
	}
}

func recordStatementHist(history *ApplyHistory, stmtType string, status string, desc string) (int, error) {
	if instance == nil {
		Connect()
	}
	conn, err := instance.pool.Acquire(context.Background())
	if checkErr(err, "Error acquiring connection for user") {
		return 0, err
	}
	defer conn.Release()
	var stmtId int
	//TODO: no need to do this check once we change the datatype to "text"
	if len(desc) > 200 {
		desc = desc[:200]
	}
	conn.QueryRow(context.Background(), histStmtInsert, history.RunId, stmtType, status, desc).Scan(&stmtId)
	stmtHist := ApplyStatement{StatementId: stmtId, StatementType: stmtType, Status: status, Description: desc}
	history.Statements = append(history.Statements, stmtHist)
	if status != "success" {
		history.Status = "failed"
	}
	return stmtId, nil
}

// Bootstrap uses the superuser to create the application role and schema, grant the role privileges on the schema and
// make the schema the role's search path. It is safe to re-run - an existing role keeps its password and the schema is
// only created if it does not exist. Any failure exits the program.
func Bootstrap(su string, supwd string) {
	username := viper.GetString("dbconn.username")
	schema := viper.GetString("dbconn.schema")
	password := viper.GetString("dbconn.password")
	if len(password) == 0 {
		fmt.Fprintln(os.Stderr, "A password for the application user is required to bootstrap - set it with --dbpassword or SKM_DBCONN_PASSWORD")
		os.Exit(1)
	}
	for _, name := range []string{username, schema} {
		if !identifierPattern.MatchString(name) {
			fmt.Fprintf(os.Stderr, "Invalid user or schema name %q - use lowercase letters, digits, _ or $ (starting with a letter or _), up to 63 characters\n", name)
			os.Exit(1)
		}
	}
	conn, err := pgx.Connect(context.Background(), connString(su, supwd))
	if checkErr(err, "Unable to connect to database:") {
		os.Exit(1)
	}
	defer conn.Close(context.Background())

	role := pgx.Identifier{username}.Sanitize()
	schemaName := pgx.Identifier{schema}.Sanitize()
	var roleExists bool
	err = conn.QueryRow(context.Background(), "select exists (select 1 from pg_roles where rolname = $1)", username).Scan(&roleExists)
	if checkErr(err, "Unable to check whether the user exists:") {
		os.Exit(1)
	}
	if roleExists {
		fmt.Printf("user %s already exists - leaving its password unchanged\n", username)
	} else {
		// create user does not accept bind parameters, so the password is escaped as a string literal instead
		escapedPwd, err := conn.PgConn().EscapeString(password)
		if checkErr(err, "Unable to escape the user password:") {
			os.Exit(1)
		}
		bootstrapExec(conn, fmt.Sprintf(bootstrapUser, role, escapedPwd), "Unable to add user:")
		fmt.Printf("user %s created\n", username)
	}
	bootstrapExec(conn, fmt.Sprintf(bootstrapSchema, schemaName, role), "Unable to create schema:")
	fmt.Printf("schema %s is ready\n", schema)
	bootstrapExec(conn, fmt.Sprintf(bootstrapGrant, schemaName, role), "Unable to apply grants:")
	fmt.Println("Grants added to schema")
	bootstrapExec(conn, fmt.Sprintf(bootstrapSearchPath, role, schemaName), "Unable to set the search path:")
	fmt.Printf("search path for user %s set to %s\n", username, schema)
}

// bootstrapExec runs a bootstrap statement, exiting on failure. The statement is never printed since it may contain
// the user's password
func bootstrapExec(conn *pgx.Conn, sql string, failMsg string) {
	_, err := conn.Exec(context.Background(), sql)
	if checkErr(err, failMsg) {
		os.Exit(1)
	}
}

func CreateHistTables() {
	if instance == nil {
		Connect()
	}
	conn, err := instance.pool.Acquire(context.Background())
	if checkErr(err, "Unable to connect to the database") {
		os.Exit(1)
	}
	defer conn.Release()
	// order matters - the statements table has a foreign key to the history table
	for _, name := range []string{"skima_schema_history", "skima_schema_statements"} {
		histTable, err := manifest.ParseTableByName(name)
		if checkErr(err, fmt.Sprintf("Unable to parse the %s table definition:", name)) {
			os.Exit(1)
		}
		_, err = conn.Exec(context.Background(), histTable.GenerateDdl())
		if checkErr(err, fmt.Sprintf("Unable to create the %s table:", name)) {
			os.Exit(1)
		}
		for _, fkDdl := range histTable.GenerateFkDdl() {
			_, err = conn.Exec(context.Background(), fkDdl)
			if pgErr := asPgError(err); pgErr != nil && pgErr.Code == "42710" {
				continue // foreign key already exists
			}
			if checkErr(err, fmt.Sprintf("Unable to add the foreign keys for the %s table:", name)) {
				os.Exit(1)
			}
		}
	}
	fmt.Println("skima history tables initialized")
}

func GetApplyHistory() (ApplyHistory, error) {
	if instance == nil {
		Connect()
	}
	hist := ApplyHistory{}
	conn, err := instance.pool.Acquire(context.Background())
	if checkErr(err, "Error acquiring connection for user") {
		return hist, err
	}
	defer conn.Release()

	var rowcount int
	err = conn.QueryRow(context.Background(), "select 1 from pg_tables where schemaname = $1 and tablename = $2", viper.GetString("dbconn.schema"), histTableName).Scan(&rowcount)
	switch err {
	case nil:
		// return the latest version of the db
		err = conn.QueryRow(context.Background(), histLatest).Scan(&hist.RunId, &hist.VersionString, &hist.ApplyTimestamp, &hist.CompletionTimestamp, &hist.ApplyType, &hist.Status, &hist.FileName)
		switch err {
		case nil:
			hist.SchemaVersion = util.ExtractVersion(hist.VersionString)
			srows, _ := conn.Query(context.Background(), histStatements, hist.RunId)
			for srows.Next() {
				var stmt = ApplyStatement{}
				var swallow int
				srows.Scan(&swallow, &stmt.StatementId, &stmt.StatementType, &stmt.Status, &stmt.Description)
				hist.Statements = append(hist.Statements, stmt)
			}
		}
	case pgx.ErrNoRows:
		return hist, NoHistTable
	default:
		fmt.Println("Unexpected error occurred")
	}
	return hist, err
}

func (hist ApplyHistory) String() string {
	var bld strings.Builder
	bld.WriteString("ApplyHistory:\n")
	fmt.Fprintf(&bld, "  Run ID: %d\n", hist.RunId)
	fmt.Fprintf(&bld, "  Version: %s\n", hist.SchemaVersion.String())
	fmt.Fprintf(&bld, "  Execution Type: %s\n", hist.ApplyType)
	fmt.Fprintf(&bld, "  Status: %s\n", hist.Status)
	fmt.Fprintf(&bld, "  Start Time: %s\n", hist.ApplyTimestamp.String())
	if !hist.CompletionTimestamp.IsZero() {
		fmt.Fprintf(&bld, "  Completion Time: %s\n", hist.CompletionTimestamp.String())
		diff := hist.CompletionTimestamp.Sub(hist.ApplyTimestamp)
		fmt.Fprintf(&bld, "  Duration (seconds): %f\n", diff.Seconds())
	}
	if len(hist.FileName) > 0 {
		fmt.Fprintf(&bld, "  Trigger File Name: %s\n", hist.FileName)
	}

	bld.WriteString("Apply Statements:\n")
	if len(hist.Statements) == 0 {
		bld.WriteString("  There were no statements applied in associated with this run")
		return bld.String()
	}
	for _, stmt := range hist.Statements {
		fmt.Fprintf(&bld, "  %s\n", stmt.String())
	}
	return bld.String()
}

func (stmt ApplyStatement) String() string {
	return fmt.Sprintf("StatementId: %d | Type: %s | Status: %s | Description: %s", stmt.StatementId, stmt.StatementType, stmt.Status, stmt.Description)
}
