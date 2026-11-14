package changes

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// repo creates a git repository with the manifest in a subdirectory - like a service repository with its schema
// alongside the code - and returns the manifest's working directory
func repo(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	root := t.TempDir()
	workdir := filepath.Join(root, "schema")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return string(out)
	}
	run("init", "-q", "-b", "main")
	return workdir, run
}

func write(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBaselineFromGit(t *testing.T) {
	workdir, run := repo(t)
	table := `{"name":"users","columns":[{"name":"id","datatype":"integer","nullable":false}]}`
	write(t, filepath.Join(workdir, "state", "users-table.json"), table)
	run("add", ".")
	run("commit", "-q", "-m", "state only")
	if _, err := ResolveBaseline(workdir, ""); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("no changeset commits yet - EXPECTED ErrNoBaseline  ACTUAL: %v", err)
	}

	write(t, filepath.Join(workdir, "deltas", "v1_0_1.json"), `{"deltas":[]}`)
	run("add", ".")
	run("commit", "-q", "-m", "changeset 1.0.1")
	write(t, filepath.Join(workdir, "README.md"), "a later commit that doesn't touch deltas/")
	run("add", ".")
	run("commit", "-q", "-m", "docs")

	baseline, err := ResolveBaseline(workdir, "")
	if err != nil || baseline.Subject != "changeset 1.0.1" || baseline.Ref != "" {
		t.Fatalf("default baseline - EXPECTED the last commit that changed deltas/  ACTUAL: %+v %v", baseline, err)
	}
	if head, err := ResolveBaseline(workdir, "HEAD"); err != nil || head.Subject != "docs" || head.Ref != "HEAD" {
		t.Errorf("explicit ref - EXPECTED HEAD  ACTUAL: %+v %v", head, err)
	}
	if _, err := ResolveBaseline(workdir, "no-such-ref"); err == nil {
		t.Error("EXPECTED an error for an unknown ref")
	}

	// edit the state in the working tree: the diff is against the baseline commit's files
	write(t, filepath.Join(workdir, "state", "users-table.json"),
		`{"name":"users","columns":[{"name":"id","datatype":"integer","nullable":false},{"name":"email","datatype":"text","nullable":true}]}`)
	write(t, filepath.Join(workdir, "state", "sql", "pre", "f.sql"), "create function f() ...")
	result, err := Diff(workdir, baseline, Draft{})
	if err != nil {
		t.Fatal(err)
	}
	expectDeltas(t, result, `{"object":"column","action":"add","name":"email","reference":"users"}`)
	if len(result.Unsupported) != 1 || result.Unsupported[0].Target != filepath.Join("state", "sql", "pre", "f.sql") {
		t.Errorf("EXPECTED the new sql file reported as unsupported  ACTUAL: %+v", result.Unsupported)
	}
	if result.Baseline.Commit != baseline.Commit || len(result.PendingChangesets) != 0 {
		t.Errorf("result - baseline %+v, pending %v", result.Baseline, result.PendingChangesets)
	}

	// an uncommitted changeset is reported as pending
	write(t, filepath.Join(workdir, "deltas", "v1_0_2.json"), `{"deltas":[]}`)
	if pending, err := PendingChangesets(workdir); err != nil || !slices.Equal(pending, []string{"v1_0_2.json"}) {
		t.Errorf("EXPECTED v1_0_2.json pending  ACTUAL: %v %v", pending, err)
	}
}

func TestBaselineOutsideGit(t *testing.T) {
	if _, err := ResolveBaseline(t.TempDir(), ""); err == nil {
		t.Error("EXPECTED an error outside a git repository")
	}
}
