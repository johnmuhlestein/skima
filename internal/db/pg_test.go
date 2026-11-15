package db

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/johnmuhlestein/skima/internal/plan"
	"github.com/johnmuhlestein/skima/internal/util"
)

// The tests in this file run against postgres - SKIMA_VERIFY_DATABASE=1 and SKM_DBCONN_* for a user that can create
// roles and schemas (the superuser in CI, see make test-db). Each test works in a schema of its own, dropped afterwards

// pgConfig is the configured database, with the schema given - it skips the test when no database is configured
func pgConfig(t *testing.T, schema string) Config {
	t.Helper()
	if os.Getenv("SKIMA_VERIFY_DATABASE") != "1" {
		t.Skip("set SKIMA_VERIFY_DATABASE=1 and SKM_DBCONN_* to run the db tests against postgres")
	}
	port, _ := strconv.Atoi(os.Getenv("SKM_DBCONN_PORT"))
	return Config{Host: os.Getenv("SKM_DBCONN_CONNSTRING"), Port: port, Database: os.Getenv("SKM_DBCONN_DATABASE"),
		Schema: schema, User: os.Getenv("SKM_DBCONN_USERNAME"), Password: os.Getenv("SKM_DBCONN_PASSWORD")}
}

// superExec runs statements as the configured user, outside any schema
func superExec(t *testing.T, cfg Config, statements ...string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), cfg.connString(cfg.User, cfg.Password))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, sql := range statements {
		if _, err := conn.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %s", sql, err)
		}
	}
}

// managedSchema creates an empty schema with skima's history tables and opens it
func managedSchema(t *testing.T, schema string) *DB {
	t.Helper()
	d := emptySchema(t, schema)
	if err := d.CreateHistTables(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	return d
}

// emptySchema creates an empty schema, dropped when the test ends, and opens it
func emptySchema(t *testing.T, schema string) *DB {
	t.Helper()
	cfg := pgConfig(t, schema)
	superExec(t, cfg, "drop schema if exists "+schema+" cascade", "create schema "+schema)
	t.Cleanup(func() { superExec(t, cfg, "drop schema if exists "+schema+" cascade") })
	d, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func queryString(t *testing.T, d *DB, sql string, args ...any) string {
	t.Helper()
	var value string
	if err := d.pool.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %s", sql, err)
	}
	return value
}

func tableExists(t *testing.T, d *DB, table string) bool {
	t.Helper()
	return queryString(t, d, "select (to_regclass($1) is not null)::text", d.Schema()+"."+table) == "true"
}

// statuses is each recorded statement of the run as "type:status", in order
func statuses(t *testing.T, d *DB, runId int) string {
	t.Helper()
	return queryString(t, d, "select coalesce(string_agg(stmt_type || ':' || status, ',' order by stmt_id), '') from skima_schema_statements where run_id = $1", runId)
}

func runStatus(t *testing.T, d *DB, runId int) string {
	t.Helper()
	return queryString(t, d, "select status from skima_schema_history where run_id = $1", runId)
}

// apply records and applies the unit as apply does, returning the failures and the run
func apply(t *testing.T, ctx context.Context, d *DB, unit plan.Unit) (int, ApplyHistory, string) {
	t.Helper()
	history, err := d.InitializeHistory(context.Background(), util.ExtractVersion("1.0.1"), "changeset", "v1_0_1.json")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	failures, err := d.ApplyUnit(ctx, &history, unit, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if history.Status == "started" {
		history.Status = "success"
	}
	if err := d.FinalizeHistory(ctx, &history); err != nil {
		t.Fatal(err)
	}
	return failures, history, out.String()
}

func changeset(transactional bool, steps ...plan.Step) plan.Unit {
	return plan.Unit{Kind: plan.UnitChangeset, Version: "1.0.1", Transactional: transactional, Steps: steps}
}

func sqlStep(name string, sql string, tolerate ...string) plan.Step {
	return plan.Step{Kind: plan.KindDelta, Describe: name, Sql: []string{sql}, Tolerate: tolerate, StopOnFailure: true,
		Record: plan.Record{Type: name, Name: name}}
}

func TestHistoryTables(t *testing.T) {
	d := emptySchema(t, "skima_db_hist")
	ctx := context.Background()
	if _, err := d.GetApplyHistory(ctx); !errors.Is(err, ErrNoHistTable) {
		t.Errorf("EXPECTED ErrNoHistTable before the tables exist  ACTUAL: %v", err)
	}
	for range 2 { // creating them again is a no-op
		if err := d.CreateHistTables(ctx, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.GetApplyHistory(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("EXPECTED no successful run yet  ACTUAL: %v", err)
	}
	if _, err := d.InitializeHistory(ctx, util.ExtractVersion("1.0.0"), "nonsense", ""); err == nil {
		t.Error("EXPECTED an invalid apply type to be refused")
	}

	failures, run, _ := apply(t, ctx, d, changeset(true, sqlStep("create t", "create table t (id int)"), sqlStep("create u", "create table u (id int)")))
	if failures != 0 {
		t.Fatalf("EXPECTED the changeset applied  ACTUAL: %d failures", failures)
	}
	latest, err := d.GetApplyHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.RunId != run.RunId || latest.VersionString != "1.0.1" || latest.Status != "success" || latest.FileName != "v1_0_1.json" || len(latest.Statements) != 2 {
		t.Errorf("EXPECTED run %d of 1.0.1 with its 2 statements  ACTUAL: %+v", run.RunId, latest)
	}

	// a failed run does not become the latest, nor does an apply --sql run
	if failures, _, _ := apply(t, ctx, d, changeset(true, sqlStep("bad", "select * from no_such_table"))); failures == 0 {
		t.Fatal("EXPECTED the changeset to fail")
	}
	sqlRun, err := d.InitializeHistory(ctx, util.ExtractVersion("1.0.2"), "sql", "")
	if err != nil {
		t.Fatal(err)
	}
	sqlRun.Status = "success"
	if err := d.FinalizeHistory(ctx, &sqlRun); err != nil {
		t.Fatal(err)
	}
	if latest, err = d.GetApplyHistory(ctx); err != nil || latest.RunId != run.RunId {
		t.Errorf("EXPECTED run %d to stay the latest  ACTUAL: run %d, %v", run.RunId, latest.RunId, err)
	}

	// descriptions are recorded whole, with invalid UTF-8 replaced
	long := strings.Repeat("é", 300) + "\xff"
	if _, err := d.recordStatementHist(ctx, &run, "note", "success", long); err != nil {
		t.Fatal(err)
	}
	recorded := queryString(t, d, "select description from skima_schema_statements where run_id = $1 and stmt_type = 'note'", run.RunId)
	if recorded != strings.Repeat("é", 300)+"�" {
		t.Errorf("EXPECTED the description recorded whole  ACTUAL: %d characters", len([]rune(recorded)))
	}
}

func TestTransactionalRollback(t *testing.T) {
	d := managedSchema(t, "skima_db_tx")
	failures, run, out := apply(t, context.Background(), d, changeset(true,
		sqlStep("create t", "create table t (id int)"),
		sqlStep("bad", "select * from no_such_table"),
		sqlStep("create u", "create table u (id int)")))
	if failures != 1 || !strings.Contains(out, "changeset 1.0.1 was rolled back") {
		t.Errorf("EXPECTED 1 failure and the changeset rolled back  ACTUAL: %d\n%s", failures, out)
	}
	if tableExists(t, d, "t") || tableExists(t, d, "u") {
		t.Error("EXPECTED no table left behind")
	}
	if got := statuses(t, d, run.RunId); got != "create t:rolled back,bad:failed" {
		t.Errorf("EXPECTED the success rolled back and the failure recorded  ACTUAL: %s", got)
	}
	if got := runStatus(t, d, run.RunId); got != "rolled back" {
		t.Errorf("EXPECTED the run rolled back  ACTUAL: %s", got)
	}
}

func TestNonTransactional(t *testing.T) {
	d := managedSchema(t, "skima_db_notx")
	failures, run, out := apply(t, context.Background(), d, changeset(false,
		sqlStep("create t", "create table t (id int)"),
		sqlStep("bad", "select * from no_such_table"),
		sqlStep("create u", "create table u (id int)")))
	if failures != 1 || !strings.Contains(out, "applied without a transaction") {
		t.Errorf("EXPECTED 1 failure, without a transaction  ACTUAL: %d\n%s", failures, out)
	}
	// what ran before the failure is kept, nothing after it runs
	if !tableExists(t, d, "t") || tableExists(t, d, "u") {
		t.Error("EXPECTED t kept and u not created")
	}
	if got := statuses(t, d, run.RunId); got != "create t:success,bad:failed" {
		t.Errorf("ACTUAL: %s", got)
	}
	if got := runStatus(t, d, run.RunId); got != "fast-fail" {
		t.Errorf("EXPECTED the run fast-fail  ACTUAL: %s", got)
	}
}

// a tolerated error rolls back to the statement's savepoint, so the transaction carries on and commits
func TestToleratedErrorInTransaction(t *testing.T) {
	d := managedSchema(t, "skima_db_savepoint")
	failures, run, out := apply(t, context.Background(), d, changeset(true,
		sqlStep("create t", "create table t (id int)"),
		sqlStep("create t again", "create table t (id int)", "42P07"),
		sqlStep("create u", "create table u (id int)")))
	if failures != 0 || !strings.Contains(out, "tolerated [42P07]") {
		t.Errorf("EXPECTED the duplicate tolerated  ACTUAL: %d\n%s", failures, out)
	}
	if !tableExists(t, d, "t") || !tableExists(t, d, "u") {
		t.Error("EXPECTED both tables committed")
	}
	if got := runStatus(t, d, run.RunId); got != "success" {
		t.Errorf("ACTUAL: %s", got)
	}
}

// deferred constraints are checked at commit - a failed commit is recorded and the run rolled back
func TestCommitFailure(t *testing.T) {
	d := managedSchema(t, "skima_db_commit")
	failures, run, _ := apply(t, context.Background(), d, changeset(true,
		sqlStep("create parent", "create table parent (id int primary key)"),
		sqlStep("create child", "create table child (parent_id int references parent deferrable initially deferred)"),
		sqlStep("orphan", "insert into child values (1)")))
	if failures != 1 {
		t.Errorf("EXPECTED the commit to fail  ACTUAL: %d failures", failures)
	}
	if tableExists(t, d, "parent") {
		t.Error("EXPECTED the changeset rolled back")
	}
	if got := statuses(t, d, run.RunId); !strings.HasSuffix(got, "Commit changeset 1.0.1:failed") {
		t.Errorf("EXPECTED the commit failure recorded  ACTUAL: %s", got)
	}
}

// a statement that cannot run in a transaction says how to opt out
func TestNotInTransaction(t *testing.T) {
	d := managedSchema(t, "skima_db_concurrently")
	failures, _, out := apply(t, context.Background(), d, changeset(true,
		sqlStep("create t", "create table t (id int)"),
		sqlStep("index", "create index concurrently t_idx on t (id)")))
	if failures != 1 || !strings.Contains(out, `set "transaction": false on the changeset`) {
		t.Errorf("EXPECTED the opt out explained  ACTUAL: %d\n%s", failures, out)
	}
	failures, _, out = apply(t, context.Background(), d, changeset(false,
		sqlStep("create t", "create table t (id int)"),
		sqlStep("index", "create index concurrently t_idx on t (id)")))
	if failures != 0 {
		t.Errorf("EXPECTED it to apply without a transaction  ACTUAL: %d\n%s", failures, out)
	}
}

// cancelling the context cancels the running statement on the server, rolls back and records the run cancelled
func TestCancelAgainstPostgres(t *testing.T) {
	d := managedSchema(t, "skima_db_cancel")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	failures, run, out := apply(t, ctx, d, changeset(true,
		sqlStep("create t", "create table t (id int)"),
		sqlStep("sleep", "select pg_sleep(30)"),
		sqlStep("create u", "create table u (id int)")))
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("EXPECTED the statement cancelled promptly  ACTUAL: %s", elapsed)
	}
	if failures == 0 || tableExists(t, d, "t") {
		t.Errorf("EXPECTED the run rolled back  ACTUAL: %d failures", failures)
	}
	if got := runStatus(t, d, run.RunId); got != "cancelled" {
		t.Errorf("EXPECTED the run cancelled  ACTUAL: %s", got)
	}
	// the server was asked to cancel the statement - the connection survives, so the transaction is rolled back on it
	if strings.Contains(out, "connection was lost") || strings.Contains(out, "Unable to roll back") {
		t.Errorf("EXPECTED the rollback to run on the connection  ACTUAL:\n%s", out)
	}
}

// release unlocks a lock that was taken - a test expecting the lock to be refused must not leak it, or closing the
// pool would wait for its connection forever
func release(unlock func(), err error) error {
	if unlock != nil {
		unlock()
	}
	return err
}

func TestApplyLock(t *testing.T) {
	first := emptySchema(t, "skima_db_lock")
	second, err := Open(first.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx := context.Background()

	unlock, err := first.LockApply(ctx, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// contention: the second apply times out, naming who holds the lock
	var out bytes.Buffer
	start := time.Now()
	err = release(second.LockApply(ctx, 300*time.Millisecond, &out))
	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") || !strings.Contains(err.Error(), "held by pid") {
		t.Errorf("EXPECTED a timeout naming the holder  ACTUAL: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("EXPECTED the timeout respected  ACTUAL: %s", elapsed)
	}
	if !strings.Contains(out.String(), "Another skima apply is running against schema skima_db_lock") {
		t.Errorf("ACTUAL: %s", out.String())
	}

	// cancelled while waiting
	cancelled, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err = release(second.LockApply(cancelled, 0, io.Discard)); err == nil || !strings.Contains(err.Error(), "interrupted while waiting") {
		t.Errorf("EXPECTED the wait interrupted  ACTUAL: %v", err)
	}

	// a waiting apply gets the lock once it is released
	got := make(chan error, 1)
	go func() {
		unlockSecond, err := second.LockApply(ctx, 10*time.Second, io.Discard)
		if err == nil {
			unlockSecond()
		}
		got <- err
	}()
	time.Sleep(200 * time.Millisecond)
	unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("EXPECTED the lock once released  ACTUAL: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EXPECTED the waiting apply to get the lock")
	}

	// a lock is per schema - another schema's apply is not blocked
	other := emptySchema(t, "skima_db_lock_other")
	unlock, err = first.LockApply(ctx, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	unlockOther, err := other.LockApply(ctx, 100*time.Millisecond, io.Discard)
	if err != nil {
		t.Errorf("EXPECTED another schema's lock to be free  ACTUAL: %v", err)
	} else {
		unlockOther()
	}
}

func TestBootstrap(t *testing.T) {
	const name = "skima_db_bootstrap"
	cfg := pgConfig(t, name)
	su, supwd := cfg.User, cfg.Password
	drop := func() { superExec(t, cfg, "drop schema if exists "+name+" cascade", "drop role if exists "+name) }
	drop()
	t.Cleanup(drop)
	app := cfg
	app.User, app.Password = name, "it's a secret"
	ctx := context.Background()

	var out bytes.Buffer
	if err := Bootstrap(ctx, app, su, supwd, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "user "+name+" created") {
		t.Errorf("ACTUAL: %s", out.String())
	}
	// the user can log in with its password, owns the schema and has it as its search path
	d, err := Open(app)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.CreateHistTables(ctx, io.Discard); err != nil {
		t.Fatalf("EXPECTED the user to create tables in its schema  ACTUAL: %v", err)
	}
	if got := queryString(t, d, "select current_user || ':' || current_schema()"); got != name+":"+name {
		t.Errorf("ACTUAL: %s", got)
	}

	// rerunning keeps the existing user's password
	out.Reset()
	rerun := app
	rerun.Password = "a different one"
	if err := Bootstrap(ctx, rerun, su, supwd, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already exists - leaving its password unchanged") {
		t.Errorf("ACTUAL: %s", out.String())
	}
	d.Close()
	if d, err = Open(app); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got := queryString(t, d, "select current_user"); got != name {
		t.Errorf("EXPECTED the original password to still work  ACTUAL: %s", got)
	}

	if err := Bootstrap(ctx, app, su, "wrong", io.Discard); err == nil || !strings.Contains(err.Error(), "as the superuser") {
		t.Errorf("EXPECTED a superuser login failure  ACTUAL: %v", err)
	}
}

// the checks that need no database
func TestBootstrapRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		err  string
	}{
		{"no password", Config{User: "app", Schema: "app"}, "a password for the application user is required"},
		{"upper case user", Config{User: "App", Schema: "app", Password: "x"}, `invalid user or schema name "App"`},
		{"quote in schema", Config{User: "app", Schema: `a"b`, Password: "x"}, `invalid user or schema name "a\"b"`},
		{"too long", Config{User: strings.Repeat("a", 64), Schema: "app", Password: "x"}, "up to 63 characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Bootstrap(context.Background(), tc.cfg, "su", "pwd", io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("EXPECTED %q  ACTUAL: %v", tc.err, err)
			}
		})
	}
}
