package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/viper"
)

// result is what a command run in-process printed and exited with
type result struct {
	code           int
	stdout, stderr string
}

// skima runs the command line in-process, as main does, with a fresh configuration: no config file (HOME is empty)
// and the working directory given
func skima(t *testing.T, workdir string, args ...string) result {
	t.Helper()
	viper.Reset()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SKM_WORKDIR", workdir)
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	code := run(context.Background(), root, args, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (r result) expect(t *testing.T, code int, contains ...string) {
	t.Helper()
	if r.code != code {
		t.Errorf("EXPECTED exit code %d  ACTUAL: %d\n--- stdout\n%s--- stderr\n%s", code, r.code, r.stdout, r.stderr)
	}
	for _, text := range contains {
		if !strings.Contains(r.stdout+r.stderr, text) {
			t.Errorf("EXPECTED the output to contain %q\n--- stdout\n%s--- stderr\n%s", text, r.stdout, r.stderr)
		}
	}
}

// manifestCopy copies the plan fixture manifest (schema 1.0.3: tables, views, a materialized view, an enum, a
// sequence and sql files) - in a git repository when commit is set, as the changeset commands need
func manifestCopy(t *testing.T, commit bool) string {
	t.Helper()
	root := t.TempDir()
	workdir := filepath.Join(root, "schema")
	if err := os.CopyFS(workdir, os.DirFS("../internal/plan/testdata/manifest")); err != nil {
		t.Fatal(err)
	}
	if commit {
		git(t, root, "init", "-q", "-b", "main")
		git(t, root, "add", ".")
		git(t, root, "commit", "-q", "-m", "schema at 1.0.3")
	}
	return workdir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

func write(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUsageErrors(t *testing.T) {
	workdir := manifestCopy(t, false)
	skima(t, workdir, "plan", "--no-such-flag").expect(t, 1, "unknown flag: --no-such-flag - see skima plan --help")
	skima(t, workdir, "plan", "extra").expect(t, 1, `unknown command "extra"`)
	skima(t, workdir, "plan", "--from", "latest").expect(t, 1, "--from must be a version")
	skima(t, workdir, "changeset", "backfill", "documents", "status").expect(t, 1, "at least one of the flags in the group [expression script] is required")
	if r := skima(t, workdir, "--version"); r.code != 0 || !strings.HasPrefix(r.stdout, "skima version ") {
		t.Errorf("--version: %+v", r)
	}
}

func TestValidate(t *testing.T) {
	workdir := manifestCopy(t, false)
	skima(t, workdir, "manifest", "validate").expect(t, 0, "ok       state/documents-table.json", "11 files checked: 11 valid, 0 invalid")

	r := skima(t, workdir, "manifest", "validate", "--output=json")
	if r.code != 0 || !strings.HasPrefix(strings.TrimSpace(r.stdout), "[") {
		t.Errorf("EXPECTED the results as JSON  ACTUAL: %+v", r)
	}

	write(t, filepath.Join(workdir, "state", "documents-table.json"), `{"name":"documents","columns":[{"name":"id","datatype":"uuid","nullabel":false}]}`)
	skima(t, workdir, "manifest", "validate").expect(t, 1, "INVALID  state/documents-table.json", "'nullabel' not allowed")
	// plan validates what apply would apply first
	skima(t, workdir, "plan", "--from", "none").expect(t, 1, "the manifest is invalid - skima apply would not apply anything")
	skima(t, workdir, "plan", "--from", "none", "--skip-validation").expect(t, 0)
}

func TestFmt(t *testing.T) {
	workdir := manifestCopy(t, false)
	skima(t, workdir, "manifest", "fmt").expect(t, 0, "already formatted")
	skima(t, workdir, "manifest", "fmt", "--check").expect(t, 0, "0 not formatted")

	path := filepath.Join(workdir, "state", "tag_color-enum.json")
	write(t, path, `{"name":"tag_color","values":["red","green"]}`)
	skima(t, workdir, "manifest", "fmt", "--check").expect(t, 1, "unformatted  state/tag_color-enum.json")
	skima(t, workdir, "manifest", "fmt").expect(t, 0, "formatted    state/tag_color-enum.json")
	skima(t, workdir, "manifest", "fmt", "--check").expect(t, 0)
}

func TestPlanOffline(t *testing.T) {
	workdir := manifestCopy(t, false)
	skima(t, workdir, "plan", "--from", "none").expect(t, 0, "create table if not exists documents")
	skima(t, workdir, "plan", "--from", "none", "--detailed-exitcode").expect(t, 2)
	skima(t, workdir, "plan", "--from", "1.0.3", "--detailed-exitcode").expect(t, 0)
	skima(t, workdir, "plan", "--from", "1.0.1", "--detailed-exitcode").expect(t, 2, "v1_0_2.json", "v1_0_3.json")

	r := skima(t, workdir, "plan", "--from", "1.0.2", "--output=json")
	if r.code != 0 || !strings.Contains(r.stdout, `"version": "1.0.3"`) {
		t.Errorf("EXPECTED the 1.0.3 changeset planned as JSON  ACTUAL: %+v", r)
	}
}

func TestManifestCommands(t *testing.T) {
	workdir := manifestCopy(t, false)
	skima(t, workdir, "manifest").expect(t, 0, "working directory: "+workdir)
	skima(t, workdir, "manifest", "list").expect(t, 0, "documents-table.json", "SQL (pre):")
	skima(t, workdir, "manifest", "list", "--delta").expect(t, 0, "v1_0_3.json")
	skima(t, workdir, "manifest", "parse", "--table", "documents").expect(t, 0, "Table DDL:", "FKs:")
	skima(t, workdir, "manifest", "parse", "--table", "nope").expect(t, 1, "unable to parse nope")
	skima(t, workdir, "manifest", "parse").expect(t, 0, "Parsing v1_0_3.json")
}

func TestChangesetCommands(t *testing.T) {
	workdir := manifestCopy(t, true)
	skima(t, workdir, "changeset", "diff").expect(t, 0, "The state files have not changed since the baseline")
	skima(t, workdir, "changeset", "diff", "--check").expect(t, 0)
	skima(t, workdir, "changeset", "build", "--description", "nothing").expect(t, 1, "there is nothing to build")
	skima(t, workdir, "changeset", "verify").expect(t, 1, "no changeset is waiting to be committed")

	skima(t, workdir, "changeset", "rename", "documents", "file_location", "storage_path").expect(t, 0,
		"updated state/documents-table.json", "Recorded: documents.file_location renamed to storage_path")
	skima(t, workdir, "changeset", "hints").expect(t, 0, "rename    documents.file_location to storage_path")
	skima(t, workdir, "changeset", "diff", "--check").expect(t, 1, "renamed  column documents.file_location - to storage_path")
	skima(t, workdir, "changeset", "diff", "--baseline", "no-such-ref").expect(t, 1, "no-such-ref")

	skima(t, workdir, "changeset", "build", "--description", "storage path").expect(t, 0, "Wrote deltas/v1_0_4.json")
	skima(t, workdir, "changeset", "hints").expect(t, 0, "No hints recorded")
	// a second build would repeat the uncommitted changeset's deltas
	skima(t, workdir, "changeset", "build", "--description", "again").expect(t, 1, "v1_0_4.json not committed yet")
	skima(t, workdir, "changeset", "verify", "v1_0_3.json").expect(t, 1, "v1_0_3.json is committed")
}

// database skips the test unless a postgres is configured - SKIMA_VERIFY_DATABASE=1 and SKM_DBCONN_* for a user that
// can create roles and schemas (the superuser in CI)
func database(t *testing.T) (host string, port int, dbname, user, password string) {
	t.Helper()
	if os.Getenv("SKIMA_VERIFY_DATABASE") != "1" {
		t.Skip("set SKIMA_VERIFY_DATABASE=1 and SKM_DBCONN_* to run the commands against postgres")
	}
	port, _ = strconv.Atoi(os.Getenv("SKM_DBCONN_PORT"))
	return os.Getenv("SKM_DBCONN_CONNSTRING"), port, os.Getenv("SKM_DBCONN_DATABASE"), os.Getenv("SKM_DBCONN_USERNAME"), os.Getenv("SKM_DBCONN_PASSWORD")
}

// bootstrapped configures a schema and role of the same name for the commands, dropped before and after the test -
// apply --bootstrap creates them. It returns a superuser connection to the database
func bootstrapped(t *testing.T, schema string) *pgx.Conn {
	t.Helper()
	host, port, dbname, superuser, superpwd := database(t)
	connString := (&url.URL{Scheme: "postgresql", User: url.UserPassword(superuser, superpwd), Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/" + dbname}).String()
	conn, err := pgx.Connect(context.Background(), connString)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		for _, sql := range []string{"drop schema if exists " + schema + " cascade", "drop role if exists " + schema} {
			if _, err := conn.Exec(context.Background(), sql); err != nil {
				t.Fatal(err)
			}
		}
	}
	cleanup()
	t.Cleanup(func() {
		cleanup()
		_ = conn.Close(context.Background())
	})
	t.Setenv("SKM_DBCONN_SCHEMA", schema)
	t.Setenv("SKM_DBCONN_USERNAME", schema)
	t.Setenv("SKM_DBCONN_PASSWORD", "cmd-test-pwd")
	t.Setenv("SKM_DBCONN_SUPERUSER", superuser)
	t.Setenv("SKM_DBCONN_SUPERUSERPWD", superpwd)
	return conn
}

func pgExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %s", sql, err)
	}
}

// TestApplyAgainstPostgres bootstraps a schema of its own, applies the state build and drifts it
func TestApplyAgainstPostgres(t *testing.T) {
	const schema = "skima_cmd_test"
	bootstrapped(t, schema)
	workdir := manifestCopy(t, false)

	skima(t, workdir, "apply", "--bootstrap").expect(t, 0, fmt.Sprintf("user %s created", schema), "skima history tables initialized")
	skima(t, workdir, "inspect").expect(t, 0, "No successful builds found")
	skima(t, workdir, "plan", "--detailed-exitcode").expect(t, 2, "create table if not exists documents")
	skima(t, workdir, "apply", "--dry-run").expect(t, 0, "create table if not exists documents")
	skima(t, workdir, "apply").expect(t, 0, "SUCCESS: Run 1 completed without failures", "Last successfully completed version: 1.0.3")
	skima(t, workdir, "plan", "--detailed-exitcode").expect(t, 0)
	skima(t, workdir, "apply").expect(t, 0, "There are no changes more current than 1.0.3")
	skima(t, workdir, "drift", "--detailed-exitcode").expect(t, 0, "No drift: schema "+schema)
	skima(t, workdir, "inspect").expect(t, 0, "Status: success")
}

// TestApplyScenariosAgainstPostgres covers what a deploy pipeline runs into: another apply holding the lock, a rerun of
// the state sql files between releases, and a broken changeset applied with --skip-validation
func TestApplyScenariosAgainstPostgres(t *testing.T) {
	const schema = "skima_cmd_scenarios"
	super := bootstrapped(t, schema)
	workdir := manifestCopy(t, false)
	skima(t, workdir, "apply", "--bootstrap").expect(t, 0)
	skima(t, workdir, "apply").expect(t, 0, "Last successfully completed version: 1.0.3")

	// another apply holds the lock - this one gives up after the timeout, having applied nothing
	pgExec(t, super, "select pg_advisory_lock(hashtext($1))", "skima apply "+schema)
	skima(t, workdir, "apply", "--lock-timeout", "300ms").expect(t, 1, "Another skima apply is running against schema "+schema,
		"timed out after 300ms waiting for the apply lock")
	pgExec(t, super, "select pg_advisory_unlock(hashtext($1))", "skima apply "+schema)

	// rerunning the state sql files records a run at the manifest's version, but a changeset after it is still applied
	skima(t, workdir, "apply", "--sql").expect(t, 0)
	write(t, filepath.Join(workdir, "deltas", "v1_0_4.json"), `{"deltas":[{"object":"script","description":"notes","sql":["create table notes (id int)"]}]}`)
	skima(t, workdir, "plan", "--detailed-exitcode").expect(t, 2, "changeset 1.0.4")
	skima(t, workdir, "apply").expect(t, 0, "Last successfully completed version: 1.0.4")

	// --skip-validation applies what validation would refuse, but a file that is not JSON still can't be planned
	next := filepath.Join(workdir, "deltas", "v1_0_5.json")
	write(t, next, `{"deltas":[{"object":"column","action":"add"`)
	skima(t, workdir, "apply").expect(t, 1, "INVALID  deltas/v1_0_5.json")
	skima(t, workdir, "apply", "--skip-validation").expect(t, 1, "unable to parse deltas/v1_0_5.json")
	skima(t, workdir, "inspect").expect(t, 0, "1.0.4")
	// a delta skima does not know generates nothing - it is recorded as skipped
	write(t, next, `{"deltas":[{"object":"bogus","action":"add","name":"x"}]}`)
	skima(t, workdir, "apply").expect(t, 1, "INVALID  deltas/v1_0_5.json")
	skima(t, workdir, "apply", "--skip-validation").expect(t, 0, "skipped due to no DDL", "Last successfully completed version: 1.0.5")
	skima(t, workdir, "inspect").expect(t, 0, "deltaSkipped")

	// a changeset that fails is rolled back and apply exits 1
	write(t, filepath.Join(workdir, "deltas", "v1_0_6.json"), `{"deltas":[{"object":"script","description":"ok","sql":["create table kept (id int)"]},{"object":"script","description":"fails","sql":["select * from no_such_table"]}]}`)
	skima(t, workdir, "apply").expect(t, 1, "no_such_table", "The changeset 1.0.6 was rolled back")
	var kept bool
	if err := super.QueryRow(context.Background(), "select to_regclass($1) is not null", schema+".kept").Scan(&kept); err != nil || kept {
		t.Errorf("EXPECTED the failed changeset's table rolled back  ACTUAL: exists %v, %v", kept, err)
	}
	skima(t, workdir, "plan", "--detailed-exitcode").expect(t, 2, "changeset 1.0.6")
}

// TestBaselineAgainstPostgres adopts a schema skima does not manage yet: --baseline records its version without
// applying anything, so only the changesets after it are applied
func TestBaselineAgainstPostgres(t *testing.T) {
	const schema = "skima_cmd_baseline"
	super := bootstrapped(t, schema)
	workdir := manifestCopy(t, false)
	skima(t, workdir, "apply", "--bootstrap").expect(t, 0)
	skima(t, workdir, "apply").expect(t, 0)
	// the schema exists, but skima has no record of it
	pgExec(t, super, "drop table "+schema+".skima_schema_statements, "+schema+".skima_schema_history")
	skima(t, workdir, "plan", "--detailed-exitcode").expect(t, 2, "create table if not exists documents")

	skima(t, workdir, "apply", "--baseline", "--baseline-version", "1.0.3").expect(t, 0)
	skima(t, workdir, "plan", "--detailed-exitcode").expect(t, 0)
	skima(t, workdir, "drift", "--detailed-exitcode").expect(t, 0)
	skima(t, workdir, "inspect").expect(t, 0, "1.0.3", "baseline")

	write(t, filepath.Join(workdir, "deltas", "v1_0_4.json"), `{"deltas":[{"object":"script","description":"notes","sql":["create table notes (id int)"]}]}`)
	skima(t, workdir, "apply").expect(t, 0, "Last successfully completed version: 1.0.4")
}
