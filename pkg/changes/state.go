// Package changes works out how the state files changed since a baseline - the state at a git commit - and generates
// the changeset deltas that make those changes (see docs/specs/state-editor-api.md)
package changes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"skima/pkg/manifest"
	"strings"
)

// State is the state files on one side of a comparison
type State struct {
	Tables map[string]manifest.Table
	// Other holds the views, enums, sequences and materialized views, keyed "kind name" - changes to them are reported
	// but no deltas are generated for them yet
	Other     map[string]any
	Sequences map[string]manifest.Sequence
	// Sql holds the state sql files by path relative to the working directory
	Sql map[string]string
}

// otherKinds are the state kinds compared but not generated
var otherKinds = map[string]manifest.FileKind{
	"-view.json":    manifest.KindView,
	"-enum.json":    manifest.KindEnum,
	"-matview.json": manifest.KindMatview,
}

// parseState builds a State from file contents keyed by path relative to the working directory (state/...)
func parseState(files map[string][]byte) (State, error) {
	state := State{Tables: map[string]manifest.Table{}, Other: map[string]any{}, Sequences: map[string]manifest.Sequence{}, Sql: map[string]string{}}
	for path, content := range files {
		base := filepath.Base(path)
		switch {
		case filepath.Dir(path) == "state" && strings.HasSuffix(base, "-table.json"):
			var t manifest.Table
			if err := json.Unmarshal(content, &t); err != nil {
				return State{}, fmt.Errorf("parsing %s: %w", path, err)
			}
			state.Tables[strings.TrimSuffix(base, "-table.json")] = t
		case filepath.Dir(path) == "state" && strings.HasSuffix(base, "-sequence.json"):
			var s manifest.Sequence
			if err := json.Unmarshal(content, &s); err != nil {
				return State{}, fmt.Errorf("parsing %s: %w", path, err)
			}
			state.Sequences[strings.TrimSuffix(base, "-sequence.json")] = s
		case filepath.Dir(path) == "state" && filepath.Ext(base) == ".json":
			for suffix, kind := range otherKinds {
				if strings.HasSuffix(base, suffix) {
					var parsed any
					if err := json.Unmarshal(content, &parsed); err != nil {
						return State{}, fmt.Errorf("parsing %s: %w", path, err)
					}
					if m, ok := parsed.(map[string]any); ok {
						delete(m, "$schema") // not part of the definition
					}
					state.Other[string(kind)+" "+strings.TrimSuffix(base, suffix)] = parsed
				}
			}
		case strings.HasPrefix(path, filepath.Join("state", "sql")+string(filepath.Separator)) && filepath.Ext(base) == ".sql":
			state.Sql[path] = string(content)
		}
	}
	return state, nil
}

// ReadWorkingState reads the state files in the working directory
func ReadWorkingState(workdir string) (State, error) {
	files := map[string][]byte{}
	root := filepath.Join(workdir, "state")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(workdir, path)
		if err != nil {
			return err
		}
		files[rel] = content
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return State{}, err
	}
	return parseState(files)
}

// Baseline is the git commit whose state files changes are measured from
type Baseline struct {
	// Ref is the ref it was resolved from - empty for the default, the last commit that changed deltas/
	Ref     string `json:"ref,omitempty"`
	Commit  string `json:"commit"`
	Subject string `json:"subject"`
}

// ErrNoBaseline is returned when no commit has changed the deltas directory and no baseline ref was given
var ErrNoBaseline = errors.New("no commit has changed deltas/ yet - pass a baseline git ref")

// git runs a git command in dir, returning its trimmed output
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveBaseline finds the baseline commit: ref when given, otherwise the last commit reachable from HEAD that
// changed the deltas directory - the state the latest changeset brought the schema to
func ResolveBaseline(workdir string, ref string) (Baseline, error) {
	if _, err := git(workdir, "rev-parse", "--show-toplevel"); err != nil {
		return Baseline{}, fmt.Errorf("the working directory must be in a git repository: %w", err)
	}
	baseline := Baseline{Ref: ref}
	if ref == "" {
		out, err := git(workdir, "log", "-1", "--format=%H", "--", "deltas")
		if err != nil {
			return Baseline{}, err
		}
		if out == "" {
			return Baseline{}, ErrNoBaseline
		}
		baseline.Commit = out
	} else {
		out, err := git(workdir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err != nil || out == "" {
			return Baseline{}, fmt.Errorf("unknown git ref %q", ref)
		}
		baseline.Commit = out
	}
	subject, err := git(workdir, "log", "-1", "--format=%s", baseline.Commit)
	if err != nil {
		return Baseline{}, err
	}
	baseline.Subject = subject
	return baseline, nil
}

// ReadBaselineState reads the state files as they were at the baseline commit
func ReadBaselineState(workdir string, baseline Baseline) (State, error) {
	files, err := baselineFiles(workdir, baseline)
	if err != nil {
		return State{}, err
	}
	return parseState(files)
}

// ExportBaselineState writes the state files as they were at the baseline commit to dir/state - a manifest that plans
// the baseline's state build
func ExportBaselineState(workdir string, baseline Baseline, dir string) error {
	files, err := baselineFiles(workdir, baseline)
	if err != nil {
		return err
	}
	for path, content := range files {
		target := filepath.Join(dir, path)
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err = os.WriteFile(target, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// baselineFiles reads the files under state/ at the baseline commit, keyed by path relative to the working directory
func baselineFiles(workdir string, baseline Baseline) (map[string][]byte, error) {
	list, err := git(workdir, "ls-tree", "-r", "--name-only", baseline.Commit, "--", "state")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, path := range strings.Split(list, "\n") {
		if path == "" {
			continue
		}
		cmd := exec.Command("git", "show", baseline.Commit+":./"+filepath.ToSlash(path))
		cmd.Dir = workdir
		content, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("reading %s at %s: %w", path, short(baseline.Commit), err)
		}
		files[filepath.FromSlash(path)] = content
	}
	return files, nil
}

// PendingChangesets lists changeset files under deltas/ that are not committed yet
func PendingChangesets(workdir string) ([]string, error) {
	out, err := git(workdir, "status", "--porcelain", "--untracked-files=all", "--", "deltas")
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if filepath.Ext(path) == ".json" && filepath.Base(filepath.Dir(path)) == "deltas" {
			pending = append(pending, filepath.Base(path))
		}
	}
	return pending, nil
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
