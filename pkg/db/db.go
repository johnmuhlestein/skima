package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
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
		// identifies skima's sessions in pg_stat_activity, e.g. who holds the apply lock
		poolConfig.ConnConfig.RuntimeParams["application_name"] = "skima"
		newPool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to connect to database:", err)
			os.Exit(1)
		}
		instance = &PgPool{pool: newPool}
	})
	return instance
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
	// how to opt out of the transaction - printed when a statement cannot run inside one
	optOut string
}

func (se savepointExecer) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	savepoint, err := se.tx.Begin(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := savepoint.Exec(ctx, sql, arguments...)
	if err != nil {
		if pgErr := asPgError(err); pgErr != nil && pgErr.Code == "25001" { // active_sql_transaction
			fmt.Fprintf(os.Stderr, "  this statement cannot run inside a transaction - %s to apply it without one\n", se.optOut)
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

// run applies a changeset or state build on one connection - in a single transaction unless it opted out
type run struct {
	conn    *pgxpool.Conn
	tx      pgx.Tx // nil when the run is applied without a transaction
	ex      execer
	history *ApplyHistory
	name    string // e.g. "changeset 1.0.1" or "state build"
}

// beginRun acquires a connection and, when inTransaction is set, starts the run's transaction. optOut tells the user
// how to apply the run without a transaction. It returns nil, with the history marked failed, if either step fails.
func beginRun(history *ApplyHistory, name string, inTransaction bool, optOut string) *run {
	conn := acquireConn()
	if conn == nil {
		history.Status = "failed"
		return nil
	}
	r := &run{conn: conn, ex: conn, history: history, name: name}
	if !inTransaction {
		fmt.Printf("The %s is applied without a transaction - a failure part way through is not rolled back\n", name)
		return r
	}
	tx, err := conn.Begin(context.Background())
	if checkErr(err, fmt.Sprintf("Unable to start the %s transaction:", name)) {
		conn.Release()
		history.Status = "failed"
		return nil
	}
	r.tx = tx
	r.ex = savepointExecer{tx: tx, optOut: optOut}
	return r
}

// finish commits the run's transaction when there were no failures, otherwise it rolls the transaction back and marks
// the run 'rolled back'. It releases the connection and returns the number of failures, including a failed commit.
func (r *run) finish(failures int) int {
	defer r.conn.Release()
	if r.tx == nil {
		return failures
	}
	defer r.tx.Rollback(context.Background()) // no-op once committed
	if failures == 0 {
		// deferred constraints are checked at commit, so a commit can still fail
		err := r.tx.Commit(context.Background())
		if err == nil {
			return 0
		}
		stmtId, err2 := recordStatementHist(r.history, "Commit "+r.name, "failed", err.Error())
		fmt.Fprintf(os.Stderr, "FAILURE: committing %s - Statement ID: %d\n %s\n", r.name, stmtId, err)
		checkErr(err2, "Unable to record the statement history for committing the "+r.name)
		failures++
	}
	if err := r.tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		if r.conn.Conn().IsClosed() {
			fmt.Fprintf(os.Stderr, "The database connection was lost - the server discards the open %s transaction\n", r.name)
		} else {
			fmt.Fprintf(os.Stderr, "Unable to roll back the %s transaction: %s\n", r.name, err)
		}
	}
	markRolledBack(r.history)
	fmt.Fprintf(os.Stderr, "The %s was rolled back - none of its changes were kept\n", r.name)
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
// deploy jobs would otherwise apply the same changesets concurrently. If another apply holds the lock, this waits up to
// timeout for it to finish (0 waits indefinitely) and exits if it does not, without applying anything. The lock is
// held on a dedicated connection until the process exits.
func LockApply(timeout time.Duration) {
	conn := acquireConn()
	if conn == nil {
		os.Exit(1)
	}
	schema := viper.GetString("dbconn.schema")
	key := "skima apply " + schema
	var locked bool
	err := conn.QueryRow(context.Background(), "select pg_try_advisory_lock(hashtext($1))", key).Scan(&locked)
	if checkErr(err, "Unable to take the apply lock:") {
		os.Exit(1)
	}
	if locked {
		return // the connection is intentionally never released - closing it at exit releases the lock
	}

	wait := "until it finishes"
	if timeout > 0 {
		wait = "up to " + timeout.String()
	}
	fmt.Printf("Another skima apply is running against schema %s%s - waiting %s\n", schema, lockHolder(conn, key), wait)
	// lock_timeout bounds the wait for the advisory lock - 0 disables it
	timeoutMs := timeout.Milliseconds()
	if timeout > 0 && timeoutMs == 0 {
		timeoutMs = 1
	}
	if _, err = conn.Exec(context.Background(), "select set_config('lock_timeout', $1, false)", strconv.FormatInt(timeoutMs, 10)); checkErr(err, "Unable to set the apply lock timeout:") {
		os.Exit(1)
	}
	_, err = conn.Exec(context.Background(), "select pg_advisory_lock(hashtext($1))", key)
	if pgErr := asPgError(err); pgErr != nil && pgErr.Code == "55P03" { // lock_not_available
		fmt.Fprintf(os.Stderr, "ERROR: timed out after %s waiting for the apply lock on schema %s%s - nothing was applied. Rerun once that apply finishes, or set a longer --lock-timeout\n", timeout, schema, lockHolder(conn, key))
		os.Exit(1)
	}
	if checkErr(err, "Unable to take the apply lock:") {
		os.Exit(1)
	}
	fmt.Println("Apply lock acquired")
	// the connection is intentionally never released - closing it at exit releases the lock
}

// lockHolder describes the session holding the apply lock, to help find a stuck apply. A bigint advisory lock key is
// stored in pg_locks as two 32 bit halves: classid (high) and objid (low)
func lockHolder(conn *pgxpool.Conn, key string) string {
	var pid int
	var application, client string
	var since time.Time
	err := conn.QueryRow(context.Background(), `select a.pid, coalesce(nullif(a.application_name, ''), 'unknown application'),
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
