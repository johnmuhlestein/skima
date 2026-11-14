package cmd

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/johnmuhlestein/skima/internal/changes"
	"github.com/johnmuhlestein/skima/internal/editor"
	"github.com/johnmuhlestein/skima/internal/manifest"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var buildOptions changes.BuildOptions
var backfillExpression, backfillScript string
var clearHints bool

// changesetBuildCmd represents the changeset build command
var changesetBuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Writes the changeset for the changes since the baseline as the next changeset.",
	Long: `Generates the changeset for how the state files changed since the baseline (see skima changeset diff), writes it
as the next changeset in the deltas directory, and clears the hints in .skima/draft.json. Commit the changeset together
with the state changes it covers - that moves the baseline.

The version is the next patch version after the latest changeset, or the next minor or major version with --bump, or
an explicit --version. The written changeset is validated as apply validates a pending changeset; if it is invalid it
is removed again.

The build is refused when a changeset is not committed yet (building again would repeat its deltas), when nothing
changed, and when some changes generate no deltas (views, enums, sequences, materialized views, state sql files) unless
--acknowledge-unsupported is given - those deltas are then left to add to the changeset by hand.

Example:
  skima changeset build --description "document status"
  skima changeset build --description "new billing tables" --bump minor
  skima changeset build --description "release candidate" --version 2.0.0-rc.1`,
	Run: func(cmd *cobra.Command, args []string) {
		workdir := viper.GetString("workdir")
		baseline, err := changes.ResolveBaseline(workdir, baselineRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		result, err := changes.Build(workdir, baseline, buildOptions)
		var unsupported *changes.UnsupportedChangesError
		var invalid *changes.InvalidChangesetError
		switch {
		case errors.As(err, &unsupported):
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			for _, c := range unsupported.Changes {
				fmt.Fprintf(os.Stderr, "  %-8s %s\n", c.Action, describeChange(c))
			}
			fmt.Fprintln(os.Stderr, "Rerun with --acknowledge-unsupported to build the rest, then add these deltas by hand")
			os.Exit(1)
		case errors.As(err, &invalid):
			fmt.Fprintln(os.Stderr, "ERROR: the generated changeset is invalid and was not written:")
			for _, issue := range invalid.Issues {
				fmt.Fprintln(os.Stderr, "  "+issue)
			}
			os.Exit(1)
		case err != nil:
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		fmt.Printf("Wrote %s - changeset %s with %d deltas\n", result.File, result.Version, len(result.Diff.Deltas))
		for _, w := range result.Diff.Warnings {
			fmt.Printf("  warning: %s\n", w)
		}
		for _, c := range result.Diff.Unsupported {
			fmt.Printf("  write by hand: %s %s\n", c.Action, describeChange(c))
		}
		fmt.Println("Commit it together with the state changes it covers. skima plan shows the SQL it runs")
	},
}

// changesetRenameCmd represents the changeset rename command
var changesetRenameCmd = &cobra.Command{
	Use:   "rename <table> <from> <to>",
	Short: "Renames a column and records the rename for the changeset.",
	Long: `Records that a column is renamed, so the changeset renames it rather than dropping and adding it - which would
lose its data.

If the table's state file still has the old name, the column is renamed there too, as the state editor renames it:
also in the table's primary key, indexes, constraints and foreign keys, in other tables' foreign keys that reference it,
and in the sequences it owns. If the state file was already renamed by hand, only the rename is recorded.

Example:
  skima changeset rename documents file_loc file_location`,
	Args: cobra.ExactArgs(3),
	Run: func(cmd *cobra.Command, args []string) {
		workdir := viper.GetString("workdir")
		table, from, to := args[0], args[1], args[2]
		ed := editor.New(workdir)
		current, etag, err := ed.Table(table)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		has := func(name string) bool {
			return slices.ContainsFunc(current.Columns, func(c manifest.Column) bool { return c.Name == name })
		}
		draft, err := changes.LoadDraft(workdir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		switch {
		case has(from):
			result, err := ed.RenameColumn(table, etag, from, to)
			if err != nil {
				fmt.Fprintln(os.Stderr, "ERROR:", err)
				os.Exit(1)
			}
			for _, file := range result.Written {
				fmt.Printf("updated %s\n", file)
			}
			for _, w := range result.Warnings {
				fmt.Printf("  warning: %s\n", w)
			}
		case has(to):
			fmt.Printf("%s.%s is already named %s in the state file - recording the rename\n", table, from, to)
		default:
			fmt.Fprintf(os.Stderr, "ERROR: table '%s' has neither column '%s' nor '%s'\n", table, from, to)
			os.Exit(1)
		}
		draft.RecordRename(table, from, to)
		if err = draft.Save(workdir); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		fmt.Printf("Recorded: %s.%s renamed to %s\n", table, from, to)
	},
}

// changesetBackfillCmd represents the changeset backfill command
var changesetBackfillCmd = &cobra.Command{
	Use:   "backfill <table> <column> (--expression <sql> | --script <file>)",
	Short: "Records how to fill a new NOT NULL column on a table that has rows.",
	Long: `Adding a NOT NULL column with no default fails on a table that has rows. With a backfill, the changeset adds the
column as nullable, fills it, then makes it NOT NULL. Fill it from a SQL expression - the changeset runs
update <table> set <column> = <expression> where <column> is null - or from a script in deltas/scripts, for
backfills that need joins or several statements.

Example:
  skima changeset backfill documents status --expression "'draft'"
  skima changeset backfill documents owner --script backfill_document_owner.sql`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		workdir := viper.GetString("workdir")
		table, column := args[0], args[1]
		if (backfillExpression == "") == (backfillScript == "") {
			fmt.Fprintln(os.Stderr, "ERROR: give exactly one of --expression or --script")
			os.Exit(1)
		}
		current, _, err := editor.New(workdir).Table(table)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		i := slices.IndexFunc(current.Columns, func(c manifest.Column) bool { return c.Name == column })
		if i < 0 {
			fmt.Fprintf(os.Stderr, "ERROR: table '%s' has no column '%s' - add it to the state file first\n", table, column)
			os.Exit(1)
		}
		if c := current.Columns[i]; c.Nullable || c.Default != "" {
			fmt.Printf("warning: %s.%s is nullable or has a default - the backfill is only used for a NOT NULL column with no default\n", table, column)
		}
		draft, err := changes.LoadDraft(workdir)
		if err == nil {
			draft.RecordBackfill(changes.BackfillHint{Table: table, Column: column, Expression: backfillExpression, Script: backfillScript})
			err = draft.Save(workdir)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		fmt.Printf("Recorded: backfill for %s.%s\n", table, column)
	},
}

// changesetHintsCmd represents the changeset hints command
var changesetHintsCmd = &cobra.Command{
	Use:   "hints",
	Short: "Lists the recorded rename and backfill hints.",
	Long: `Lists the hints in .skima/draft.json - the renames and backfills the next changeset uses. --clear removes them
all, e.g. to treat a rename as a drop and an add after all. skima changeset build clears them once it has used them.`,
	Run: func(cmd *cobra.Command, args []string) {
		workdir := viper.GetString("workdir")
		if clearHints {
			if err := changes.ClearDraft(workdir); err != nil {
				fmt.Fprintln(os.Stderr, "ERROR:", err)
				os.Exit(1)
			}
			fmt.Println("Cleared the hints")
			return
		}
		draft, err := changes.LoadDraft(workdir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		if len(draft.Renames) == 0 && len(draft.Backfills) == 0 {
			fmt.Println("No hints recorded")
			return
		}
		for _, r := range draft.Renames {
			fmt.Printf("rename    %s.%s to %s\n", r.Table, r.From, r.To)
		}
		for _, b := range draft.Backfills {
			from := "expression " + b.Expression
			if b.Script != "" {
				from = "script deltas/scripts/" + b.Script
			}
			fmt.Printf("backfill  %s.%s from %s\n", b.Table, b.Column, from)
		}
	},
}

func init() {
	changesetCmd.AddCommand(changesetBuildCmd, changesetRenameCmd, changesetBackfillCmd, changesetHintsCmd)
	changesetBuildCmd.Flags().StringVar(&buildOptions.Description, "description", "", "What the changeset does")
	changesetBuildCmd.Flags().StringVar(&buildOptions.Bump, "bump", "patch", "The part of the latest changeset version to increase: major, minor or patch")
	changesetBuildCmd.Flags().StringVar(&buildOptions.Version, "version", "", "An explicit version, e.g. 1.1.0 or 1.1.0-rc.1, instead of --bump")
	changesetBuildCmd.Flags().BoolVar(&buildOptions.NoTransaction, "no-transaction", false, `Write "transaction": false, for statements that can't run in a transaction`)
	changesetBuildCmd.Flags().BoolVar(&buildOptions.AcknowledgeUnsupported, "acknowledge-unsupported", false, "Build although some changes generate no deltas - add those deltas by hand")
	cobra.CheckErr(changesetBuildCmd.MarkFlagRequired("description"))
	changesetBackfillCmd.Flags().StringVar(&backfillExpression, "expression", "", "A SQL expression to fill the column with")
	changesetBackfillCmd.Flags().StringVar(&backfillScript, "script", "", "A sql file in deltas/scripts that fills the column")
	changesetHintsCmd.Flags().BoolVar(&clearHints, "clear", false, "Remove every hint")
}
