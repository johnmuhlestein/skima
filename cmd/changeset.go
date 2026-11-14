package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"skima/pkg/changes"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var baselineRef string
var diffCheck bool

// changesetCmd groups the commands that generate changesets from the state files
var changesetCmd = &cobra.Command{
	Use:   "changeset",
	Short: "Generates changesets from changes to the state files.",
	Long: `Works out how the state files changed since a baseline - the state at a git commit - and generates the changeset
deltas that make those changes, so a schema change is made once, in the state files.

The baseline is by default the last commit that changed the deltas directory: the state the latest changeset brought
the schema to. Use --baseline to compare with another git ref, e.g. origin/main. The working directory must be in a
git repository.

A diff can't tell a renamed column from a dropped and an added one, or how to fill a new NOT NULL column on a table
that has rows. That intent is kept as hints in .skima/draft.json, which skima git-ignores.`,
}

// changesetDiffCmd represents the changeset diff command
var changesetDiffCmd = &cobra.Command{
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

Example:
  skima changeset diff
  skima changeset diff --baseline origin/main
  skima changeset diff --output=json
  skima changeset diff --check`,
	Run: func(cmd *cobra.Command, args []string) {
		workdir := viper.GetString("workdir")
		baseline, err := changes.ResolveBaseline(workdir, baselineRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		draft, err := changes.LoadDraft(workdir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR: unable to read", changes.DraftPath(workdir)+":", err)
			os.Exit(1)
		}
		result, err := changes.Diff(workdir, baseline, draft)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		statements, planErr := changes.PreviewSql(result.Deltas)
		if output == "json" {
			report := struct {
				changes.Result
				Sql []string `json:"sql"`
			}{result, statements}
			jsonOutput, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				fmt.Fprintln(os.Stderr, "Unable to marshal the diff into JSON", err)
				os.Exit(1)
			}
			fmt.Println(string(jsonOutput))
		} else if !diffCheck || result.HasChanges() {
			printDiff(result, statements, planErr)
		}
		if diffCheck && result.HasChanges() {
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(changesetCmd)
	changesetCmd.PersistentFlags().StringVar(&baselineRef, "baseline", "", "The git ref to compare with - by default the last commit that changed the deltas directory")
	changesetCmd.AddCommand(changesetDiffCmd)
	changesetDiffCmd.Flags().BoolVar(&diffCheck, "check", false, "Show nothing unless the state changed since the baseline - the exit code is 1 when it did")
}

func printDiff(result changes.Result, statements []string, planErr error) {
	from := "the last commit that changed deltas/"
	if result.Baseline.Ref != "" {
		from = result.Baseline.Ref
	}
	fmt.Printf("Baseline: %.7s %q (%s)\n", result.Baseline.Commit, result.Baseline.Subject, from)
	if len(result.PendingChangesets) > 0 {
		fmt.Printf("Not committed yet: %s - the baseline moves when a changeset is committed, so commit it with the state changes it covers\n",
			strings.Join(result.PendingChangesets, ", "))
	}
	if !result.HasChanges() {
		fmt.Println("\nThe state files have not changed since the baseline")
		return
	}
	if len(result.Changes) > 0 {
		fmt.Println("\nChanges:")
		for _, c := range result.Changes {
			fmt.Printf("  %-8s %s\n", c.Action, describeChange(c))
		}
	}
	if len(result.Unsupported) > 0 {
		fmt.Println("\nUnsupported - write these deltas by hand:")
		for _, c := range result.Unsupported {
			fmt.Printf("  %-8s %s\n", c.Action, describeChange(c))
		}
	}
	if len(result.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range result.Warnings {
			fmt.Printf("  - %s\n", w)
		}
	}
	if len(result.Deltas) > 0 {
		content, err := changes.Changeset{Deltas: result.Deltas}.Format()
		if err == nil {
			fmt.Printf("\nChangeset (%d deltas):\n%s", len(result.Deltas), content)
		}
		fmt.Println("\nSQL:")
		if planErr != nil {
			fmt.Printf("  unable to plan the changeset: %s\n", planErr)
		}
		for _, s := range statements {
			fmt.Printf("  %s;\n", strings.TrimSuffix(strings.TrimSpace(s), ";"))
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
