package changes

import (
	"errors"
	"os"
	"path/filepath"
	"skima/pkg/manifest"
	"strings"
	"testing"
)

const usersTable = `{"name":"users","columns":[{"name":"id","datatype":"integer","nullable":false},{"name":"email","datatype":"text","nullable":true}],"primary-key":["id"]}`

// buildRepo is a repository at changeset 1.0.1, with the users table
func buildRepo(t *testing.T) (string, func(args ...string) string, Baseline) {
	t.Helper()
	workdir, run := repo(t)
	write(t, filepath.Join(workdir, "state", "users-table.json"), usersTable)
	write(t, filepath.Join(workdir, "deltas", "v1_0_1.json"), `{"deltas":[{"object":"table","action":"add","name":"users"}]}`)
	run("add", ".")
	run("commit", "-q", "-m", "changeset 1.0.1")
	baseline, err := ResolveBaseline(workdir, "")
	if err != nil {
		t.Fatal(err)
	}
	return workdir, run, baseline
}

func addColumn(t *testing.T, workdir string, column string) {
	t.Helper()
	write(t, filepath.Join(workdir, "state", "users-table.json"), strings.Replace(usersTable, `],"primary-key"`, `,`+column+`],"primary-key"`, 1))
}

func TestBuild(t *testing.T) {
	workdir, _, baseline := buildRepo(t)
	if _, err := Build(workdir, baseline, BuildOptions{Description: "nothing"}); !errors.Is(err, ErrNoChanges) {
		t.Errorf("no changes - EXPECTED ErrNoChanges  ACTUAL: %v", err)
	}

	addColumn(t, workdir, `{"name":"status","datatype":"text","nullable":false}`)
	draft := Draft{Backfills: []BackfillHint{{Table: "users", Column: "status", Expression: "'active'"}}}
	if err := draft.Save(workdir); err != nil {
		t.Fatal(err)
	}
	result, err := Build(workdir, baseline, BuildOptions{Description: "user status"})
	if err != nil {
		t.Fatal(err)
	}
	if result.File != filepath.Join("deltas", "v1_0_2.json") || result.Version != "1.0.2" || len(result.Diff.Deltas) != 3 {
		t.Errorf("EXPECTED v1_0_2.json with the 3 backfill deltas  ACTUAL: %s %s %d", result.File, result.Version, len(result.Diff.Deltas))
	}
	written, err := os.ReadFile(filepath.Join(workdir, result.File))
	if err != nil || string(written) != string(result.Content) || !strings.Contains(string(written), `"description": "user status"`) {
		t.Errorf("EXPECTED the changeset written with its description  ACTUAL: %s %v", written, err)
	}
	if v := manifest.ValidateFile(filepath.Join(workdir, result.File), manifest.KindChangeset); !v.Valid {
		t.Errorf("the written changeset is invalid: %+v", v.Issues)
	}
	if _, err = os.Stat(DraftPath(workdir)); !os.IsNotExist(err) {
		t.Error("EXPECTED the draft cleared after the build")
	}

	// the changeset isn't committed yet, so another build would repeat its deltas
	var pending *PendingChangesetsError
	if _, err = Build(workdir, baseline, BuildOptions{Description: "again"}); !errors.As(err, &pending) || pending.Files[0] != "v1_0_2.json" {
		t.Errorf("EXPECTED a PendingChangesetsError for v1_0_2.json  ACTUAL: %v", err)
	}
}

func TestBuildVersions(t *testing.T) {
	cases := []struct {
		opts BuildOptions
		file string
		err  string
	}{
		{BuildOptions{Bump: "minor"}, "v1_1_0.json", ""},
		{BuildOptions{Bump: "major"}, "v2_0_0.json", ""},
		{BuildOptions{Version: "1.5.0-rc.2"}, "v1_5_0-rc_2.json", ""},
		{BuildOptions{Version: "1.0.1"}, "", "not newer than the latest changeset, 1.0.1"},
		{BuildOptions{Version: "1.0.0"}, "", "not newer"},
		{BuildOptions{Version: "soon"}, "", "is not a version"},
		{BuildOptions{Bump: "build"}, "", "unknown version part"},
	}
	for _, c := range cases {
		workdir, _, baseline := buildRepo(t)
		addColumn(t, workdir, `{"name":"age","datatype":"integer","nullable":true}`)
		c.opts.Description = "versions"
		result, err := Build(workdir, baseline, c.opts)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%+v - EXPECTED an error containing %q  ACTUAL: %v", c.opts, c.err, err)
		case c.err == "" && (err != nil || result.File != filepath.Join("deltas", c.file)):
			t.Errorf("%+v - EXPECTED %s  ACTUAL: %s %v", c.opts, c.file, result.File, err)
		}
	}
}

func TestBuildUnsupported(t *testing.T) {
	workdir, _, baseline := buildRepo(t)
	write(t, filepath.Join(workdir, "state", "active-view.json"), `{"name":"active","columns":["id"],"sql":"select id from users"}`)
	var unsupported *UnsupportedChangesError
	if _, err := Build(workdir, baseline, BuildOptions{Description: "view"}); !errors.As(err, &unsupported) || unsupported.Changes[0].Target != "active" {
		t.Errorf("EXPECTED an UnsupportedChangesError for the view  ACTUAL: %v", err)
	}
	// acknowledged, but nothing to generate
	if _, err := Build(workdir, baseline, BuildOptions{Description: "view", AcknowledgeUnsupported: true}); !errors.Is(err, ErrNothingGenerated) {
		t.Errorf("EXPECTED ErrNothingGenerated  ACTUAL: %v", err)
	}
	addColumn(t, workdir, `{"name":"age","datatype":"integer","nullable":true}`)
	result, err := Build(workdir, baseline, BuildOptions{Description: "view and column", AcknowledgeUnsupported: true, NoTransaction: true})
	if err != nil || len(result.Diff.Deltas) != 1 || !strings.Contains(string(result.Content), `"transaction": false`) {
		t.Errorf("EXPECTED the column delta built without a transaction  ACTUAL: %s %v", result.Content, err)
	}
}

func TestBuildInvalidChangesetRemoved(t *testing.T) {
	workdir, _, baseline := buildRepo(t)
	addColumn(t, workdir, `{"name":"status","datatype":"text","nullable":false}`)
	// the backfill script doesn't exist in deltas/scripts, so the changeset fails validation
	draft := Draft{Backfills: []BackfillHint{{Table: "users", Column: "status", Script: "missing.sql"}}}
	if err := draft.Save(workdir); err != nil {
		t.Fatal(err)
	}
	var invalid *InvalidChangesetError
	if _, err := Build(workdir, baseline, BuildOptions{Description: "status"}); !errors.As(err, &invalid) || !strings.Contains(err.Error(), "missing.sql") {
		t.Fatalf("EXPECTED an InvalidChangesetError naming missing.sql  ACTUAL: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "deltas", "v1_0_2.json")); !os.IsNotExist(err) {
		t.Error("EXPECTED the invalid changeset removed")
	}
	if _, err := os.Stat(DraftPath(workdir)); err != nil {
		t.Error("EXPECTED the draft kept when the build fails")
	}
}
