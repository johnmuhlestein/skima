package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/johnmuhlestein/skima/internal/db"
	"github.com/johnmuhlestein/skima/internal/drift"
	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newDriftCmd(root *rootOptions) *cobra.Command {
	var ignore []string
	var detailedExitcode, skipValidation bool
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Reports where the database schema differs from its state files.",
		Long: `Compares the schema in the database with the schema its state files describe, and reports every difference:
  missing  the state files define it, the database does not have it
  extra    the database has it, the state files do not define it
  changed  both have it, with different definitions - shown as expected and actual

Tables, columns (type, nullability, default, identity, generated, comment), primary key, unique, check and foreign key
constraints, indexes, triggers, views, materialized views (definition and indexes), enums (their values, in order),
sequences (settings and owner) and table, view, materialized view, enum and sequence comments are compared. Sequences of
identity columns are compared as the column, and a sequence a column owns (e.g. a serial column's) is not reported as
extra on its own. Functions, grants and data are not.

Postgres rewrites definitions when it stores them (varchar is stored as character varying, and check constraints and
views are reformatted), so drift does not compare the state files' text. It builds the state files into temporary
tables in a transaction that is always rolled back, then compares how postgres describes those with how it describes
the real schema. This changes nothing, takes no apply lock and only needs the TEMP privilege every user has by default.
The post sql files are not run - they load data.

The state files describe the latest changeset version. If changesets newer than the deployed version have not been
applied, the differences they make show as drift - a warning lists them.

Objects that are deliberately not in the state files can be ignored with patterns (path.Match syntax) matched
against the object's name - "table", "table.column", "table.index" and so on - or against its table:
  drift:
    ignore:
      - legacy_*                     # every table starting legacy_, and everything on them
      - documents.tags_label_idx     # an index created by a script delta
Patterns from --ignore are added to those in the config (drift.ignore, or SKM_DRIFT_IGNORE separated by spaces).

Like apply, drift validates the state files first - use --skip-validation to compare anyway.

The exit code is 0, or 1 on an error. With --detailed-exitcode it is 2 when there is drift.
Use the global --output=json flag for machine readable results.

Example:
  skima drift
  skima drift --detailed-exitcode
  skima drift --ignore 'legacy_*' --output=json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			report := drift.Report{Schema: viper.GetString("dbconn.schema")}
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()
			hist, err := database.GetApplyHistory(ctx)
			switch {
			case err == nil:
				report.Deployed = hist.SchemaVersion.String()
				pending, err := pendingChangesets(hist.SchemaVersion)
				if err != nil {
					return err
				}
				for _, path := range pending {
					report.Pending = append(report.Pending, util.ExtractVersion(filepath.Base(path)).String())
				}
			case errors.Is(err, db.ErrNoHistTable), errors.Is(err, pgx.ErrNoRows):
				// not managed by skima yet, e.g. before a baseline - it can still be compared with the state files
			default:
				return fmt.Errorf("unable to retrieve the apply history: %w", err)
			}

			if err = validateFor(cmd, skipValidation, nil, "drift cannot be checked", false); err != nil {
				return err
			}
			p, err := planner().ForState(true)
			if err != nil {
				return fmt.Errorf("unable to read the state files: %w", err)
			}
			actual, expected, err := database.ReadSchemas(ctx, p.Units[0].WithoutPostSql())
			if err != nil {
				return err
			}
			report.Findings = drift.Compare(expected, actual)
			if err = report.Ignore(append(viper.GetStringSlice("drift.ignore"), ignore...)); err != nil {
				return err
			}

			if root.json() {
				jsonOutput, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return fmt.Errorf("unable to marshal the drift report into JSON: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(jsonOutput))
			} else {
				drift.Render(cmd.OutOrStdout(), report)
			}
			if detailedExitcode && report.Drifted() {
				return exitCode(2)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&ignore, "ignore", nil, "Leave out differences in objects matching this pattern (e.g. 'legacy_*' or 'documents.*_idx') - repeatable, added to drift.ignore in the config")
	cmd.Flags().BoolVar(&detailedExitcode, "detailed-exitcode", false, "Exit with 2 when there is drift (0 when there is none, 1 on an error)")
	cmd.Flags().BoolVar(&skipValidation, "skip-validation", false, "Compare without first validating the state files against the skima JSON schemas")
	return cmd
}
