package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/johnmuhlestein/skima/internal/changes"
	"github.com/johnmuhlestein/skima/internal/drift"
	"github.com/johnmuhlestein/skima/internal/verify"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// newChangesetCmd groups the commands that generate changesets from the state files
func newChangesetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "changeset",
		Short: "Generates changesets from changes to the state files.",
		Long: `Works out how the state files changed since a baseline - the state at a git commit - and generates the changeset
deltas that make those changes, so a schema change is made once, in the state files.

The baseline is by default the last commit that changed the deltas directory: the state the latest changeset brought
the schema to. Use --baseline to compare with another git ref, e.g. origin/main. The working directory must be in a
git repository.

A diff can't tell a renamed column from a dropped and an added one, or how to fill a new NOT NULL column on a table
that has rows. That intent is kept as hints in .skima/draft.json, which skima git-ignores. For any other intent, edit the
file skima changeset build writes and prove the edit with skima changeset verify.`,
	}
	cmd.PersistentFlags().String("baseline", "", "The git ref to compare with - by default the last commit that changed the deltas directory")
	return cmd
}

// resolveBaseline resolves the changeset commands' --baseline, by default the last commit that changed deltas/
func resolveBaseline(cmd *cobra.Command) (changes.Baseline, error) {
	ref, err := cmd.Flags().GetString("baseline")
	if err != nil {
		return changes.Baseline{}, err
	}
	return changes.ResolveBaseline(viper.GetString("workdir"), ref)
}

func newChangesetDiffCmd(root *rootOptions) *cobra.Command {
	var check, verifyChanges bool
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Shows how the state changed since the baseline, and the changeset that makes the changes.",
		Long: `Shows how the state files changed since the baseline and the changeset deltas generated for those changes,
with warnings for anything risky (dropping data, changing a datatype, NOT NULL on a table with rows) and the SQL the
changeset would run. Nothing is written.

Tables, columns, primary keys, foreign keys, indexes, constraints, triggers and table and column comments generate
deltas. Changes to views, enums, sequences, materialized views and state sql files are listed as unsupported - their
deltas are written by hand for now.

With --check nothing is shown unless the state changed: the exit code is 1 when it did, for CI - a state change
pushed without its changeset.

With --verify the changeset is proven against the database configured for skima: the baseline's state files are built
and the changeset applied on top, and the result must be exactly what the current state files build. Both builds run
in a scratch schema created inside a transaction that is always rolled back - nothing is changed and no apply lock is
taken, but the database user needs the CREATE privilege on the database. The exit code is 1 when it doesn't verify.

Example:
  skima changeset diff
  skima changeset diff --baseline origin/main
  skima changeset diff --output=json
  skima changeset diff --check
  skima changeset diff --verify`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			workdir := viper.GetString("workdir")
			baseline, err := resolveBaseline(cmd)
			if err != nil {
				return err
			}
			draft, err := changes.LoadDraft(workdir)
			if err != nil {
				return fmt.Errorf("unable to read %s: %w", changes.DraftPath(workdir), err)
			}
			result, err := changes.Diff(workdir, baseline, draft)
			if err != nil {
				return err
			}
			statements, planErr := changes.PreviewSql(workdir, result.Deltas)
			var verified *verify.Report
			if verifyChanges && len(result.Deltas) > 0 {
				report, err := verify.Deltas(cmd.Context(), workdir, baseline, result.Deltas)
				if err != nil {
					return fmt.Errorf("unable to verify the changeset: %w", err)
				}
				report.Unsupported = result.Unsupported
				verified = &report
			}
			out := cmd.OutOrStdout()
			if root.json() {
				report := struct {
					changes.Result
					Sql    []string       `json:"sql"`
					Verify *verify.Report `json:"verify,omitempty"`
				}{result, statements, verified}
				jsonOutput, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return fmt.Errorf("unable to marshal the diff into JSON: %w", err)
				}
				fmt.Fprintln(out, string(jsonOutput))
			} else if !check || result.HasChanges() {
				printDiff(out, result, statements, planErr)
				if verified != nil {
					printVerify(out, *verified)
				}
			}
			if (check && result.HasChanges()) || (verified != nil && !verified.Verified) {
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&verifyChanges, "verify", false, "Prove the changeset against the database: the baseline's state with the changeset applied must equal the current state - the exit code is 1 when it doesn't")
	cmd.Flags().BoolVar(&check, "check", false, "Show nothing unless the state changed since the baseline - the exit code is 1 when it did")
	return cmd
}

func newChangesetVerifyCmd(root *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "verify [changeset]",
		Short: "Proves a changeset not committed yet against the database - e.g. one edited by hand after changeset build.",
		Long: `Proves a changeset file against the database configured for skima, as changeset diff --verify proves the
generated one: the baseline's state files are built and the changeset applied on top, and the result must be exactly
what the current state files build. The exit code is 1 when it doesn't verify.

This is how a generated changeset's intent is changed: changeset build writes it, you edit deltas/vX_Y_Z.json - a drop
and an add made a rename, a script delta that migrates data, steps split or reordered - and verify proves the edited
changeset still brings the baseline to the current state. Then commit it with the state changes it covers. While it
isn't committed, changeset build won't write another, so the edits can't be overwritten.

Verify proves the resulting schema, not the data: it builds into an empty scratch schema, so a rename and a drop and
an add both verify. Whether the data survives is for you to review - for renames and NOT NULL backfills, the hints
(skima changeset rename and backfill) generate the right deltas without hand edits.

With no changeset named, the one changeset not committed yet is verified. The file is validated as apply validates
it first. Both builds run in a scratch schema created inside a transaction that is always rolled back - nothing is
changed and no apply lock is taken, but the database user needs the CREATE privilege on the database.

Example:
  skima changeset verify
  skima changeset verify v1_0_4.json
  skima changeset verify --output=json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workdir := viper.GetString("workdir")
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			path, err := changes.PendingChangeset(workdir, name)
			if err != nil {
				return err
			}
			baseline, err := resolveBaseline(cmd)
			if err != nil {
				return err
			}
			report, err := verify.File(cmd.Context(), workdir, baseline, path)
			var invalid *changes.InvalidChangesetError
			switch {
			case errors.As(err, &invalid):
				return fmt.Errorf("%s is invalid - fix it before verifying:\n  %s", filepath.Base(path), strings.Join(invalid.Issues, "\n  "))
			case err != nil:
				return fmt.Errorf("unable to verify the changeset: %w", err)
			}
			out := cmd.OutOrStdout()
			if root.json() {
				jsonOutput, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return fmt.Errorf("unable to marshal the verification into JSON: %w", err)
				}
				fmt.Fprintln(out, string(jsonOutput))
			} else {
				from := "the last commit that changed deltas/"
				if baseline.Ref != "" {
					from = baseline.Ref
				}
				fmt.Fprintf(out, "Baseline: %.7s %q (%s)\n", baseline.Commit, baseline.Subject, from)
				fmt.Fprintf(out, "Changeset: %s (%d deltas)\n", report.File, report.Deltas)
				printVerify(out, report)
			}
			if !report.Verified {
				return exitCode(1)
			}
			return nil
		},
	}
}

func printDiff(out io.Writer, result changes.Result, statements []string, planErr error) {
	from := "the last commit that changed deltas/"
	if result.Baseline.Ref != "" {
		from = result.Baseline.Ref
	}
	fmt.Fprintf(out, "Baseline: %.7s %q (%s)\n", result.Baseline.Commit, result.Baseline.Subject, from)
	if len(result.PendingChangesets) > 0 {
		fmt.Fprintf(out, "Not committed yet: %s - the baseline moves when a changeset is committed, so commit it with the state changes it covers\n",
			strings.Join(result.PendingChangesets, ", "))
	}
	if !result.HasChanges() {
		fmt.Fprintln(out, "\nThe state files have not changed since the baseline")
		return
	}
	if len(result.Changes) > 0 {
		fmt.Fprintln(out, "\nChanges:")
		for _, c := range result.Changes {
			fmt.Fprintf(out, "  %-8s %s\n", c.Action, describeChange(c))
		}
	}
	if len(result.Unsupported) > 0 {
		fmt.Fprintln(out, "\nUnsupported - write these deltas by hand:")
		for _, c := range result.Unsupported {
			fmt.Fprintf(out, "  %-8s %s\n", c.Action, describeChange(c))
		}
	}
	if len(result.Warnings) > 0 {
		fmt.Fprintln(out, "\nWarnings:")
		for _, w := range result.Warnings {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	if len(result.Deltas) > 0 {
		content, err := changes.Changeset{Deltas: result.Deltas}.Format()
		if err == nil {
			fmt.Fprintf(out, "\nChangeset (%d deltas):\n%s", len(result.Deltas), content)
		}
		fmt.Fprintln(out, "\nSQL:")
		if planErr != nil {
			fmt.Fprintf(out, "  unable to plan the changeset: %s\n", planErr)
		}
		for _, s := range statements {
			fmt.Fprintf(out, "  %s;\n", strings.TrimSuffix(strings.TrimSpace(s), ";"))
		}
	}
}

// printVerify shows whether the changeset brings the baseline to the current state - differences as skima drift shows
// them, with "expected" from the current state files and "actual" from the baseline with the changeset applied
func printVerify(out io.Writer, report verify.Report) {
	fmt.Fprintln(out, "\nVerify:")
	switch {
	case report.ApplyError != "":
		fmt.Fprintln(out, "  NOT VERIFIED - the changeset failed to apply to the baseline:")
		for _, line := range strings.Split(report.ApplyError, "\n") {
			fmt.Fprintln(out, "    "+line)
		}
	case report.Verified:
		fmt.Fprintln(out, "  verified - the baseline with the changeset applied is exactly the current state")
	default:
		differences := fmt.Sprintf("%d differences", len(report.Findings))
		if len(report.Findings) == 1 {
			differences = "1 difference"
		}
		fmt.Fprintf(out, "  NOT VERIFIED - %s between the current state (expected) and the baseline with the changeset applied (actual):\n", differences)
		var rendered strings.Builder
		drift.Render(&rendered, drift.Report{Findings: report.Findings})
		lines := strings.Split(strings.TrimRight(rendered.String(), "\n"), "\n")
		for _, line := range lines[1:] { // the first line is drift's summary
			fmt.Fprintln(out, "  "+line)
		}
		if len(report.Unsupported) > 0 {
			fmt.Fprintln(out, "  Unsupported changes generate no deltas, so they show up here until their deltas are written by hand")
		}
	}
}

func describeChange(c changes.Change) string {
	s := c.Object + " " + c.Target
	if c.Detail != "" {
		s += " - " + c.Detail
	}
	return s
}
