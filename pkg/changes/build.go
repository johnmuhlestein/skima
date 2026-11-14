package changes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"skima/pkg/manifest"
	"skima/pkg/util"
	"strings"
)

// BuildOptions say how to write the changeset
type BuildOptions struct {
	Description string
	// Bump is major, minor or patch (the default) - the part of the latest changeset version to increase. Ignored
	// when Version is set
	Bump string
	// Version is an explicit version, e.g. 1.1.0 or 1.1.0-rc.1 - it must be newer than every changeset
	Version string
	// NoTransaction writes "transaction": false, for changesets with statements that can't run in a transaction
	NoTransaction bool
	// AcknowledgeUnsupported builds the changeset although some changes generate no deltas - they are left to write
	// by hand
	AcknowledgeUnsupported bool
}

// BuildResult is the changeset that was written
type BuildResult struct {
	// File is the changeset file, relative to the working directory
	File    string
	Version string
	Content []byte
	Diff    Result
}

var (
	// ErrNoChanges is returned when the state files have not changed since the baseline
	ErrNoChanges = errors.New("the state files have not changed since the baseline - there is nothing to build")
	// ErrNothingGenerated is returned when every change is unsupported, so no delta could be generated
	ErrNothingGenerated = errors.New("no deltas were generated - every change needs its delta written by hand")
)

// PendingChangesetsError is returned when a changeset is not committed yet - the baseline only moves when it is, so
// building again would generate its deltas a second time
type PendingChangesetsError struct {
	Files []string
}

func (e *PendingChangesetsError) Error() string {
	return fmt.Sprintf("%s not committed yet - commit it with the state changes it covers (or remove it) before building another changeset", strings.Join(e.Files, ", "))
}

// UnsupportedChangesError is returned when some changes generate no deltas and that was not acknowledged
type UnsupportedChangesError struct {
	Changes []Change
}

func (e *UnsupportedChangesError) Error() string {
	return fmt.Sprintf("%d changes generate no deltas and need them written by hand - acknowledge them to build the rest", len(e.Changes))
}

// InvalidChangesetError is returned when a changeset doesn't validate - Build removes the one it wrote again
type InvalidChangesetError struct {
	Issues []string
}

func (e *InvalidChangesetError) Error() string {
	return "the changeset is invalid: " + strings.Join(e.Issues, "; ")
}

// LatestVersion returns the highest changeset version in the working directory - 1.0.0 when there are none, as
// manifest.Version does
func LatestVersion(workdir string) (util.SemanticVersion, error) {
	files, err := manifest.ListFiles(filepath.Join(workdir, "deltas"), "json")
	if err != nil {
		return util.SemanticVersion{}, err
	}
	latest := util.ExtractVersion("1.0.0")
	for i, path := range files {
		if v := util.ExtractVersion(filepath.Base(path)); i == 0 || v.Compare(latest) > 0 {
			latest = v
		}
	}
	return latest, nil
}

// Build generates the changeset for the changes since the baseline, writes it as the next changeset, validates it as
// apply would, and clears the draft's hints
func Build(workdir string, baseline Baseline, opts BuildOptions) (BuildResult, error) {
	pending, err := PendingChangesets(workdir)
	if err != nil {
		return BuildResult{}, err
	}
	if len(pending) > 0 {
		return BuildResult{}, &PendingChangesetsError{Files: pending}
	}
	draft, err := LoadDraft(workdir)
	if err != nil {
		return BuildResult{}, err
	}
	diff, err := Diff(workdir, baseline, draft)
	if err != nil {
		return BuildResult{}, err
	}
	if !diff.HasChanges() {
		return BuildResult{}, ErrNoChanges
	}
	if len(diff.Unsupported) > 0 && !opts.AcknowledgeUnsupported {
		return BuildResult{}, &UnsupportedChangesError{Changes: diff.Unsupported}
	}
	if len(diff.Deltas) == 0 {
		return BuildResult{}, ErrNothingGenerated
	}

	latest, err := LatestVersion(workdir)
	if err != nil {
		return BuildResult{}, err
	}
	var version util.SemanticVersion
	if opts.Version != "" {
		version = util.ExtractVersion(opts.Version)
		if version.String() != strings.TrimPrefix(opts.Version, "v") {
			return BuildResult{}, fmt.Errorf("%q is not a version - use e.g. 1.1.0 or 1.1.0-rc.1", opts.Version)
		}
		if version.Compare(latest) <= 0 {
			return BuildResult{}, fmt.Errorf("version %s is not newer than the latest changeset, %s", version.String(), latest.String())
		}
	} else {
		bump := opts.Bump
		if bump == "" {
			bump = "patch"
		}
		if version, err = latest.Bump(bump); err != nil {
			return BuildResult{}, err
		}
	}
	path := filepath.Join(workdir, "deltas", version.FileName())
	if _, err = os.Stat(path); err == nil {
		return BuildResult{}, fmt.Errorf("%s already exists", filepath.Join("deltas", version.FileName()))
	}

	changeset := Changeset{Description: opts.Description, Deltas: diff.Deltas}
	if opts.NoTransaction {
		noTransaction := false
		changeset.Transaction = &noTransaction
	}
	content, err := changeset.Format()
	if err != nil {
		return BuildResult{}, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return BuildResult{}, err
	}
	if err = util.WriteFileAtomic(path, content, 0o644); err != nil {
		return BuildResult{}, err
	}
	if err = ValidateChangeset(workdir, path); err != nil {
		os.Remove(path)
		return BuildResult{}, err
	}
	if err = ClearDraft(workdir); err != nil {
		return BuildResult{}, err
	}
	return BuildResult{File: filepath.Join("deltas", version.FileName()), Version: version.String(), Content: content, Diff: diff}, nil
}

// ValidateChangeset validates a changeset file as apply validates a pending changeset: schema, file name and
// references, all as errors - an *InvalidChangesetError lists them
func ValidateChangeset(workdir, path string) error {
	results, err := manifest.ValidateForApply(workdir, []string{path})
	if err != nil {
		return err
	}
	var issues []string
	for _, r := range results {
		for _, issue := range r.Issues {
			if issue.Severity == manifest.SeverityError {
				rel, _ := filepath.Rel(workdir, r.File)
				issues = append(issues, fmt.Sprintf("%s %s: %s", rel, issue.Location, issue.Message))
			}
		}
	}
	if len(issues) > 0 {
		return &InvalidChangesetError{Issues: issues}
	}
	return nil
}
