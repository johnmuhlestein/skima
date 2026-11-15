// Package verify proves a generated changeset against a database: building the baseline's state files and applying
// the changeset on top must give exactly the schema a build of the current state files gives (see
// docs/specs/state-editor-api.md). Both builds run in a scratch schema inside a transaction that is always rolled back -
// nothing is changed and no apply lock is taken, but the database user needs the CREATE privilege.
//
// Verify proves the resulting schema, not the data: the scratch schema has no rows, so a column renamed and a column
// dropped and added again both verify. Keeping the data is what the rename and backfill hints are for
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/johnmuhlestein/skima/internal/changes"
	"github.com/johnmuhlestein/skima/internal/db"
	"github.com/johnmuhlestein/skima/internal/drift"
	"github.com/johnmuhlestein/skima/internal/plan"
)

// Report is the outcome of verifying a changeset
type Report struct {
	Baseline changes.Baseline `json:"baseline"`
	// File is the changeset file verified, relative to the working directory - empty for a generated changeset
	File   string `json:"file,omitempty"`
	Deltas int    `json:"deltas"`
	// Verified is true when the baseline with the changeset applied is exactly the current state
	Verified bool `json:"verified"`
	// Findings are the differences, as skima drift reports them: "expected" is the current state files, "actual" the
	// baseline with the changeset applied
	Findings []drift.Finding `json:"findings"`
	// ApplyError is set when the changeset failed to apply to the baseline
	ApplyError string `json:"applyError,omitempty"`
	// Unsupported changes generate no deltas, so they show up as differences until their deltas are written by hand
	Unsupported []changes.Change `json:"unsupported,omitempty"`
}

// Run verifies the changeset generated for the changes since the baseline
func Run(ctx context.Context, database *db.DB, workdir string, baseline changes.Baseline, draft changes.Draft) (Report, error) {
	diff, err := changes.Diff(workdir, baseline, draft)
	if err != nil {
		return Report{}, err
	}
	report, err := Deltas(ctx, database, workdir, baseline, diff.Deltas)
	report.Unsupported = diff.Unsupported
	return report, err
}

// Deltas verifies a changeset made of the deltas against the baseline
func Deltas(ctx context.Context, database *db.DB, workdir string, baseline changes.Baseline, deltas []changes.Delta) (Report, error) {
	scratch, err := os.MkdirTemp("", "skima-verify-")
	if err != nil {
		return Report{Baseline: baseline, Deltas: len(deltas), Findings: []drift.Finding{}}, err
	}
	defer os.RemoveAll(scratch)
	content, err := changes.Changeset{Description: "verify", Deltas: deltas}.Format()
	if err != nil {
		return Report{Baseline: baseline, Deltas: len(deltas), Findings: []drift.Finding{}}, err
	}
	changesetPath := filepath.Join(scratch, "verify.json")
	if err = os.WriteFile(changesetPath, content, 0o644); err != nil {
		return Report{Baseline: baseline, Deltas: len(deltas), Findings: []drift.Finding{}}, err
	}
	return changesetFile(ctx, database, workdir, baseline, changesetPath, len(deltas))
}

// File verifies a changeset file - typically the one changeset build wrote, edited by hand to change its intent. The
// file is validated as apply validates it first: an *changes.InvalidChangesetError lists the problems
func File(ctx context.Context, database *db.DB, workdir string, baseline changes.Baseline, path string) (Report, error) {
	rel, err := filepath.Rel(workdir, path)
	if err != nil {
		rel = path
	}
	report := Report{Baseline: baseline, File: rel, Findings: []drift.Finding{}}
	if err = changes.ValidateChangeset(workdir, path); err != nil {
		return report, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	var changeset changes.Changeset
	if err = json.Unmarshal(content, &changeset); err != nil {
		return report, err
	}
	report, err = changesetFile(ctx, database, workdir, baseline, path, len(changeset.Deltas))
	report.File = rel
	return report, err
}

// changesetFile verifies the changeset file against the baseline. The changeset reads its definitions and scripts
// from the current state files and deltas/scripts, as apply's would
func changesetFile(ctx context.Context, database *db.DB, workdir string, baseline changes.Baseline, changesetPath string, deltas int) (Report, error) {
	report := Report{Baseline: baseline, Deltas: deltas, Findings: []drift.Finding{}}
	scratch, err := os.MkdirTemp("", "skima-verify-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(scratch)

	// the baseline's state build, planned from its state files exported from git
	baselineDir := filepath.Join(scratch, "baseline")
	if err = changes.ExportBaselineState(workdir, baseline, baselineDir); err != nil {
		return report, err
	}
	baselineBuild, err := stateBuild(plan.Planner{Workdir: baselineDir})
	if err != nil {
		return report, fmt.Errorf("planning the baseline's state build: %w", err)
	}
	current := plan.Planner{Workdir: workdir}
	changeset, _, err := current.ForChangesetFile(changesetPath)
	if err != nil {
		return report, fmt.Errorf("planning the changeset: %w", err)
	}
	currentBuild, err := stateBuild(current)
	if err != nil {
		return report, fmt.Errorf("planning the current state build: %w", err)
	}

	afterChangeset, expected, err := database.VerifyChangeset(ctx, baselineBuild, changeset, currentBuild)
	var failed *db.ShadowBuildError
	if errors.As(err, &failed) && failed.Changeset {
		// the changeset itself failed - that is the verification's finding, not an error running it
		report.ApplyError = failed.Messages
		return report, nil
	}
	if err != nil {
		return report, err
	}
	report.Findings = drift.Compare(expected, afterChangeset)
	report.Verified = len(report.Findings) == 0
	return report, nil
}

// stateBuild plans the manifest's state build without its post sql files, which load data
func stateBuild(planner plan.Planner) (plan.Unit, error) {
	p, err := planner.ForState(true)
	if err != nil {
		return plan.Unit{}, err
	}
	if len(p.Units) != 1 {
		return plan.Unit{}, fmt.Errorf("expected one state build unit, got %d", len(p.Units))
	}
	return p.Units[0].WithoutPostSql(), nil
}
