// Package verify proves a generated changeset against a database: building the baseline's state files and applying
// the changeset on top must give exactly the schema a build of the current state files gives (see
// docs/specs/state-editor-api.md). Both are shadow builds, as skima drift's - nothing is changed, no apply lock is
// taken, and only the TEMP privilege is needed
package verify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"skima/pkg/changes"
	"skima/pkg/db"
	"skima/pkg/drift"
	"skima/pkg/plan"
)

// Report is the outcome of verifying a changeset
type Report struct {
	Baseline changes.Baseline `json:"baseline"`
	Deltas   int              `json:"deltas"`
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
func Run(workdir string, baseline changes.Baseline, draft changes.Draft) (Report, error) {
	diff, err := changes.Diff(workdir, baseline, draft)
	if err != nil {
		return Report{}, err
	}
	report, err := Deltas(workdir, baseline, diff.Deltas)
	report.Unsupported = diff.Unsupported
	return report, err
}

// Deltas verifies a changeset made of the deltas against the baseline
func Deltas(workdir string, baseline changes.Baseline, deltas []changes.Delta) (Report, error) {
	report := Report{Baseline: baseline, Deltas: len(deltas), Findings: []drift.Finding{}}
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
	var baselineBuild, changeset, currentBuild plan.Unit
	err = plan.InWorkdir(baselineDir, func() error {
		var err error
		baselineBuild, err = stateBuild()
		return err
	})
	if err != nil {
		return report, fmt.Errorf("planning the baseline's state build: %w", err)
	}

	// the changeset reads its definitions from the current state files, as apply's would
	content, err := changes.Changeset{Description: "verify", Deltas: deltas}.Format()
	if err != nil {
		return report, err
	}
	changesetPath := filepath.Join(scratch, "verify.json")
	if err = os.WriteFile(changesetPath, content, 0o644); err != nil {
		return report, err
	}
	err = plan.InWorkdir(workdir, func() error {
		var err error
		if changeset, _, err = plan.ForChangesetFile(changesetPath); err != nil {
			return fmt.Errorf("planning the changeset: %w", err)
		}
		if currentBuild, err = stateBuild(); err != nil {
			return fmt.Errorf("planning the current state build: %w", err)
		}
		return nil
	})
	if err != nil {
		return report, err
	}

	afterChangeset, expected, err := db.VerifyChangeset(baselineBuild, changeset, currentBuild)
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

// stateBuild plans the configured working directory's state build without its post sql files, which load data
func stateBuild() (plan.Unit, error) {
	p, err := plan.ForState(true)
	if err != nil {
		return plan.Unit{}, err
	}
	if len(p.Units) != 1 {
		return plan.Unit{}, fmt.Errorf("expected one state build unit, got %d", len(p.Units))
	}
	return p.Units[0].WithoutPostSql(), nil
}
