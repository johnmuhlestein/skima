// Package db applies planned runs to postgres and records them in skima's history tables, takes the apply lock,
// bootstraps a schema, and reads schemas for skima drift and changeset verification. Every function takes a context:
// cancelling it (Ctrl+C) stops the statement running, rolls back the run's transaction and records the run as
// cancelled. Errors are returned, never handled by exiting
package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/johnmuhlestein/skima/internal/manifest"
	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/spf13/viper"
)

const histInsert = "insert into skima_schema_history (schema_version, apply_type, status, delta_file) values ($1,$2,$3,$4) returning run_id"
const histUpdateComp = "update skima_schema_history set status = $1, completion_timestamp = now() where run_id = $2"

// an apply --sql run only reruns the state sql files - it applies no changesets, so it never decides the deployed
// version. Counting it would make the changesets newer than the deployed version look applied
const histLatest = "select run_id, schema_version, apply_start_timestamp, completion_timestamp, apply_type, status, coalesce(delta_file, '') from skima_schema_history where run_id = (select max(run_id) from skima_schema_history where status in ('complete','success') and apply_type <> 'sql')"
const histStatements = "select run_id, stmt_id, stmt_type, status, coalesce(description,'') from skima_schema_statements where run_id = $1 order by stmt_id"

const histStmtRolledBack = "update skima_schema_statements set status = 'rolled back' where run_id = $1 and status = 'success'"

const histStmtInsert = "insert into skima_schema_statements (run_id, stmt_type, status, description) values ($1,$2,$3,$4) returning stmt_id"

const bootstrapUser = "create user %s with password '%s'"
const bootstrapSchema = "create schema if not exists %s authorization %s"
const bootstrapGrant = "grant all privileges on schema %s to %s with grant option"
const bootstrapSearchPath = "alter role %s set search_path to %s"

const histTableName = "skima_schema_history"

var histApplyTypes = util.FromSlice([]string{"state", "baseline", "delta", "changeset", "sql"})
var once sync.Once
var instance *pgxpool.Pool
var poolErr error

// ErrNoHistTable is returned when the schema has no history tables - it is not managed by skima yet
var ErrNoHistTable = errors.New("history tables have not been provisioned")

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

// pool returns the connection pool for the configured database, created on first use. The pool connects lazily, so a
// database that can't be reached is reported by the first query
func pool() (*pgxpool.Pool, error) {
	once.Do(func() {
		poolConfig, err := pgxpool.ParseConfig(connString(viper.GetString("dbconn.username"), viper.GetString("dbconn.password")))
		if err != nil {
			poolErr = fmt.Errorf("invalid database connection settings: %w", err)
			return
		}
		// set as a runtime param rather than a URL "options" query value, which newer pgx versions reject
		poolConfig.ConnConfig.RuntimeParams["search_path"] = viper.GetString("dbconn.schema")
		// identifies skima's sessions in pg_stat_activity, e.g. who holds the apply lock
		poolConfig.ConnConfig.RuntimeParams["application_name"] = "skima"
		// on Ctrl+C, ask the server to cancel the running statement instead of closing the connection - the
		// connection survives, so the run's transaction is rolled back and its history recorded on it
		poolConfig.ConnConfig.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
			return &pgconn.CancelRequestContextWatcherHandler{Conn: conn, DeadlineDelay: 10 * time.Second}
		}
		instance, poolErr = pgxpool.NewWithConfig(context.Background(), poolConfig)
		if poolErr != nil {
			poolErr = fmt.Errorf("unable to connect to the database: %w", poolErr)
		}
	})
	return instance, poolErr
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

// cleanupTimeout bounds the history writes and rollbacks that run after the context is cancelled (Ctrl+C) - they
// still need to happen, but must not hang
const cleanupTimeout = 30 * time.Second

// cleanup returns a context for work that must finish even when ctx was cancelled: rolling back, recording the
// history. Call cancel when done
func cleanup(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// execer runs a statement - either a pooled connection or a run's transaction
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// savepointExecer runs each statement of a transaction in its own savepoint. An error aborts a postgres transaction,
// but some errors are tolerated (e.g. "already exists", a sql delta's bypass codes, or a view whose relation is not
// created yet) - rolling back to the savepoint undoes just the failed statement so the rest of the transaction can
// continue
type savepointExecer struct {
	tx pgx.Tx
	// how to opt out of the transaction - added to the error when a statement cannot run inside one
	optOut string
}

func (se savepointExecer) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	savepoint, err := se.tx.Begin(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := savepoint.Exec(ctx, sql, arguments...)
	if err != nil {
		rollbackCtx, cancel := cleanup(ctx)
		_ = savepoint.Rollback(rollbackCtx)
		cancel()
		if pgErr := asPgError(err); pgErr != nil && pgErr.Code == "25001" { // active_sql_transaction
			err = fmt.Errorf("%w\n  this statement cannot run inside a transaction - %s to apply it without one", err, se.optOut)
		}
		return tag, err
	}
	return tag, savepoint.Commit(ctx)
}

func acquireConn(ctx context.Context) (*pgxpool.Conn, error) {
	p, err := pool()
	if err != nil {
		return nil, err
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to the database: %w", err)
	}
	return conn, nil
}

// run applies a changeset or state build on one connection - in a single transaction unless it opted out
type run struct {
	conn    *pgxpool.Conn
	tx      pgx.Tx // nil when the run is applied without a transaction
	ex      execer
	history *ApplyHistory
	name    string // e.g. "changeset 1.0.1" or "state build"
	out     io.Writer
	errOut  io.Writer
}

// beginRun acquires a connection and, when inTransaction is set, starts the run's transaction. optOut tells the user
// how to apply the run without a transaction
func beginRun(ctx context.Context, history *ApplyHistory, name string, inTransaction bool, optOut string, out, errOut io.Writer) (*run, error) {
	conn, err := acquireConn(ctx)
	if err != nil {
		return nil, err
	}
	r := &run{conn: conn, ex: conn, history: history, name: name, out: out, errOut: errOut}
	if !inTransaction {
		fmt.Fprintf(out, "The %s is applied without a transaction - a failure part way through is not rolled back\n", name)
		return r, nil
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, fmt.Errorf("unable to start the %s transaction: %w", name, err)
	}
	r.tx = tx
	r.ex = savepointExecer{tx: tx, optOut: optOut}
	return r, nil
}

// finish commits the run's transaction when there were no failures, otherwise it rolls the transaction back and marks
// the run 'rolled back' ('cancelled' when ctx was cancelled). It releases the connection and returns the number of
// failures, including a failed commit.
func (r *run) finish(ctx context.Context, failures int) int {
	defer r.conn.Release()
	if r.tx == nil {
		if ctx.Err() != nil {
			r.history.Status = "cancelled"
		}
		return failures
	}
	done, cancel := cleanup(ctx)
	defer cancel()
	defer func() { _ = r.tx.Rollback(done) }() // no-op once committed
	if failures == 0 && ctx.Err() == nil {
		// deferred constraints are checked at commit, so a commit can still fail
		err := r.tx.Commit(ctx)
		if err == nil {
			return 0
		}
		stmtId, err2 := recordStatementHist(done, r.history, "Commit "+r.name, "failed", err.Error())
		fmt.Fprintf(r.errOut, "FAILURE: committing %s - Statement ID: %d\n %s\n", r.name, stmtId, err)
		if err2 != nil {
			fmt.Fprintf(r.errOut, "Unable to record the statement history for committing the %s: %s\n", r.name, err2)
		}
		failures++
	}
	if err := r.tx.Rollback(done); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		if r.conn.Conn().IsClosed() {
			fmt.Fprintf(r.errOut, "The database connection was lost - the server discards the open %s transaction\n", r.name)
		} else {
			fmt.Fprintf(r.errOut, "Unable to roll back the %s transaction: %s\n", r.name, err)
		}
	}
	if err := markRolledBack(done, r.history); err != nil {
		fmt.Fprintln(r.errOut, "Unable to mark the run's statements as rolled back:", err)
	}
	if ctx.Err() != nil {
		r.history.Status = "cancelled"
	}
	fmt.Fprintf(r.errOut, "The %s was rolled back - none of its changes were kept\n", r.name)
	return max(failures, 1)
}

// markRolledBack records that a run's changes were undone - statements recorded as successful did not persist
func markRolledBack(ctx context.Context, history *ApplyHistory) error {
	history.Status = "rolled back"
	conn, err := acquireConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	_, err = conn.Exec(ctx, histStmtRolledBack, history.RunId)
	return err
}

// LockApply takes a postgres advisory lock for the schema so that only one apply runs against it at a time - two
// deploy jobs would otherwise apply the same changesets concurrently. If another apply holds the lock, this waits up to
// timeout for it to finish (0 waits indefinitely) and returns an error if it does not, or if ctx is cancelled. The lock
// is held on a dedicated connection until the process exits.
func LockApply(ctx context.Context, timeout time.Duration, out io.Writer) error {
	conn, err := acquireConn(ctx)
	if err != nil {
		return err
	}
	schema := viper.GetString("dbconn.schema")
	key := "skima apply " + schema
	var locked bool
	if err = conn.QueryRow(ctx, "select pg_try_advisory_lock(hashtext($1))", key).Scan(&locked); err != nil {
		conn.Release()
		return fmt.Errorf("unable to take the apply lock: %w", err)
	}
	if locked {
		return nil // the connection is intentionally never released - closing it at exit releases the lock
	}

	wait := "until it finishes"
	if timeout > 0 {
		wait = "up to " + timeout.String()
	}
	fmt.Fprintf(out, "Another skima apply is running against schema %s%s - waiting %s\n", schema, lockHolder(ctx, conn, key), wait)
	// lock_timeout bounds the wait for the advisory lock - 0 disables it
	timeoutMs := timeout.Milliseconds()
	if timeout > 0 && timeoutMs == 0 {
		timeoutMs = 1
	}
	if _, err = conn.Exec(ctx, "select set_config('lock_timeout', $1, false)", strconv.FormatInt(timeoutMs, 10)); err != nil {
		conn.Release()
		return fmt.Errorf("unable to set the apply lock timeout: %w", err)
	}
	_, err = conn.Exec(ctx, "select pg_advisory_lock(hashtext($1))", key)
	switch pgErr := asPgError(err); {
	case pgErr != nil && pgErr.Code == "55P03": // lock_not_available
		holder := lockHolder(context.WithoutCancel(ctx), conn, key)
		conn.Release()
		return fmt.Errorf("timed out after %s waiting for the apply lock on schema %s%s - nothing was applied. Rerun once that apply finishes, or set a longer --lock-timeout", timeout, schema, holder)
	case ctx.Err() != nil:
		conn.Release()
		return fmt.Errorf("interrupted while waiting for the apply lock - nothing was applied: %w", ctx.Err())
	case err != nil:
		conn.Release()
		return fmt.Errorf("unable to take the apply lock: %w", err)
	}
	fmt.Fprintln(out, "Apply lock acquired")
	return nil // the connection is intentionally never released - closing it at exit releases the lock
}

// lockHolder describes the session holding the apply lock, to help find a stuck apply. A bigint advisory lock key is
// stored in pg_locks as two 32 bit halves: classid (high) and objid (low)
func lockHolder(ctx context.Context, conn *pgxpool.Conn, key string) string {
	var pid int
	var application, client string
	var since time.Time
	err := conn.QueryRow(ctx, `select a.pid, coalesce(nullif(a.application_name, ''), 'unknown application'),
			coalesce(host(a.client_addr), 'local'), coalesce(a.backend_start, now())
		from pg_locks l join pg_stat_activity a on a.pid = l.pid
		where l.locktype = 'advisory' and l.granted and l.objsubid = 1
			and l.classid = ((hashtext($1)::bigint >> 32) & 4294967295)::oid
			and l.objid = (hashtext($1)::bigint & 4294967295)::oid
		limit 1`, key).Scan(&pid, &application, &client, &since)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" (held by pid %d: %s on %s, connected for %s)", pid, application, client, time.Since(since).Round(time.Second))
}

// InitializeHistory creates a new history record in the database, which generates a runId and populates the basic
// information. The filename parameter is optional, and passing in an empty string will leave the field blank in
// the database
func InitializeHistory(ctx context.Context, version util.SemanticVersion, applyType string, filename string) (ApplyHistory, error) {
	hist := ApplyHistory{SchemaVersion: version, ApplyType: applyType, Status: "started", VersionString: version.String(), FileName: filename}
	if !histApplyTypes.Has(applyType) {
		return hist, fmt.Errorf("invalid apply type for the history table: %s", applyType)
	}
	if applyType == "baseline" {
		hist.Status = "complete"
	}
	conn, err := acquireConn(ctx)
	if err != nil {
		return hist, err
	}
	defer conn.Release()
	err = conn.QueryRow(ctx, histInsert, hist.SchemaVersion.String(), hist.ApplyType, hist.Status, hist.FileName).Scan(&hist.RunId)
	if err != nil {
		return hist, fmt.Errorf("unable to record the run in the history: %w", err)
	}
	return hist, nil
}

// FinalizeHistory records the run's status and completion time. It runs even when ctx was cancelled - the history
// must say how the run ended
func FinalizeHistory(ctx context.Context, history *ApplyHistory) error {
	done, cancel := cleanup(ctx)
	defer cancel()
	conn, err := acquireConn(done)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err = conn.Exec(done, histUpdateComp, history.Status, history.RunId); err != nil {
		return fmt.Errorf("unable to finalize the history record: %w", err)
	}
	return nil
}

func recordStatementHist(ctx context.Context, history *ApplyHistory, stmtType string, status string, desc string) (int, error) {
	conn, err := acquireConn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	var stmtId int
	// the description column is text, so it is recorded whole - and invalid UTF-8 (e.g. from a sql file in another
	// encoding) is replaced, since postgres would reject the row
	desc = strings.ToValidUTF8(desc, "�")
	err = conn.QueryRow(ctx, histStmtInsert, history.RunId, stmtType, status, desc).Scan(&stmtId)
	stmtHist := ApplyStatement{StatementId: stmtId, StatementType: stmtType, Status: status, Description: desc}
	history.Statements = append(history.Statements, stmtHist)
	if status == "failed" {
		history.Status = "failed"
	}
	return stmtId, err
}

// Bootstrap uses the superuser to create the application role and schema, grant the role privileges on the schema and
// make the schema the role's search path. It is safe to re-run - an existing role keeps its password and the schema is
// only created if it does not exist.
func Bootstrap(ctx context.Context, su string, supwd string, out io.Writer) error {
	username := viper.GetString("dbconn.username")
	schema := viper.GetString("dbconn.schema")
	password := viper.GetString("dbconn.password")
	if len(password) == 0 {
		return errors.New("a password for the application user is required to bootstrap - set it with SKM_DBCONN_PASSWORD or --dbpassword")
	}
	for _, name := range []string{username, schema} {
		if !identifierPattern.MatchString(name) {
			return fmt.Errorf("invalid user or schema name %q - use lowercase letters, digits, _ or $ (starting with a letter or _), up to 63 characters", name)
		}
	}
	conn, err := pgx.Connect(ctx, connString(su, supwd))
	if err != nil {
		return fmt.Errorf("unable to connect to the database as the superuser: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	role := pgx.Identifier{username}.Sanitize()
	schemaName := pgx.Identifier{schema}.Sanitize()
	var roleExists bool
	if err = conn.QueryRow(ctx, "select exists (select 1 from pg_roles where rolname = $1)", username).Scan(&roleExists); err != nil {
		return fmt.Errorf("unable to check whether the user exists: %w", err)
	}
	if roleExists {
		fmt.Fprintf(out, "user %s already exists - leaving its password unchanged\n", username)
	} else {
		// create user does not accept bind parameters, so the password is escaped as a string literal instead
		escapedPwd, err := conn.PgConn().EscapeString(password)
		if err != nil {
			return fmt.Errorf("unable to escape the user password: %w", err)
		}
		// the statement is never put in an error, since it contains the password
		if _, err = conn.Exec(ctx, fmt.Sprintf(bootstrapUser, role, escapedPwd)); err != nil {
			return fmt.Errorf("unable to add the user: %w", err)
		}
		fmt.Fprintf(out, "user %s created\n", username)
	}
	for _, step := range []struct{ sql, failure, done string }{
		{fmt.Sprintf(bootstrapSchema, schemaName, role), "unable to create the schema", fmt.Sprintf("schema %s is ready", schema)},
		{fmt.Sprintf(bootstrapGrant, schemaName, role), "unable to apply the grants", "Grants added to schema"},
		{fmt.Sprintf(bootstrapSearchPath, role, schemaName), "unable to set the search path", fmt.Sprintf("search path for user %s set to %s", username, schema)},
	} {
		if _, err = conn.Exec(ctx, step.sql); err != nil {
			return fmt.Errorf("%s: %w", step.failure, err)
		}
		fmt.Fprintln(out, step.done)
	}
	return nil
}

// CreateHistTables creates skima's history tables in the schema, if they don't exist yet
func CreateHistTables(ctx context.Context, out io.Writer) error {
	conn, err := acquireConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// order matters - the statements table has a foreign key to the history table
	for _, name := range []string{"skima_schema_history", "skima_schema_statements"} {
		histTable, err := manifest.ParseTableByName(name)
		if err != nil {
			return fmt.Errorf("unable to parse the %s table definition: %w", name, err)
		}
		if _, err = conn.Exec(ctx, histTable.GenerateDdl()); err != nil {
			return fmt.Errorf("unable to create the %s table: %w", name, err)
		}
		for _, fkDdl := range histTable.GenerateFkDdl() {
			_, err = conn.Exec(ctx, fkDdl)
			if pgErr := asPgError(err); pgErr != nil && pgErr.Code == "42710" {
				continue // foreign key already exists
			}
			if err != nil {
				return fmt.Errorf("unable to add the foreign keys for the %s table: %w", name, err)
			}
		}
	}
	fmt.Fprintln(out, "skima history tables initialized")
	return nil
}

// GetApplyHistory reads the most recent successful run and its statements - ErrNoHistTable when the schema has no
// history tables, pgx.ErrNoRows when no run has succeeded
func GetApplyHistory(ctx context.Context) (ApplyHistory, error) {
	hist := ApplyHistory{}
	conn, err := acquireConn(ctx)
	if err != nil {
		return hist, err
	}
	defer conn.Release()

	var rowcount int
	err = conn.QueryRow(ctx, "select 1 from pg_tables where schemaname = $1 and tablename = $2", viper.GetString("dbconn.schema"), histTableName).Scan(&rowcount)
	switch err {
	case nil:
		// return the latest version of the db
		err = conn.QueryRow(ctx, histLatest).Scan(&hist.RunId, &hist.VersionString, &hist.ApplyTimestamp, &hist.CompletionTimestamp, &hist.ApplyType, &hist.Status, &hist.FileName)
		switch err {
		case nil:
			hist.SchemaVersion = util.ExtractVersion(hist.VersionString)
			srows, err := conn.Query(ctx, histStatements, hist.RunId)
			if err != nil {
				return hist, fmt.Errorf("reading the statements of run %d: %w", hist.RunId, err)
			}
			defer srows.Close()
			for srows.Next() {
				var stmt = ApplyStatement{}
				var swallow int
				if err := srows.Scan(&swallow, &stmt.StatementId, &stmt.StatementType, &stmt.Status, &stmt.Description); err != nil {
					return hist, fmt.Errorf("reading the statements of run %d: %w", hist.RunId, err)
				}
				hist.Statements = append(hist.Statements, stmt)
			}
			if err := srows.Err(); err != nil {
				return hist, fmt.Errorf("reading the statements of run %d: %w", hist.RunId, err)
			}
		}
	case pgx.ErrNoRows:
		return hist, ErrNoHistTable
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
