package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"skima/pkg/db"
	"skima/pkg/drift"
	"skima/pkg/plan"
	"skima/pkg/util"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var driftIgnore []string

// driftCmd represents the drift command
var driftCmd = &cobra.Command{
	Use:   "drift",
	Short: "Reports where the database schema differs from its state files.",
	Long: `Compares the schema in the database with the schema its state files describe, and reports every difference:
  missing  the state files define it, the database does not have it
  extra    the database has it, the state files do not define it
  changed  both have it, with different definitions - shown as expected and actual

Tables, columns (type, nullability, default, identity, generated, comment), primary key, unique, check and foreign
key constraints, indexes, triggers, views, enums (their values, in order) and table, view and enum comments are
compared. Functions, grants and data are not.

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
	Run: func(cmd *cobra.Command, args []string) {
		report := drift.Report{Schema: viper.GetString("dbconn.schema")}
		hist, err := db.GetApplyHistory()
		switch err {
		case nil:
			report.Deployed = hist.SchemaVersion.String()
			for _, path := range pendingChangesets(hist.SchemaVersion) {
				report.Pending = append(report.Pending, util.ExtractVersion(filepath.Base(path)).String())
			}
		case db.NoHistTable, pgx.ErrNoRows:
			// not managed by skima yet, e.g. before a baseline - it can still be compared with the state files
		default:
			fmt.Fprintln(os.Stderr, "Unable to retrieve the apply history:", err)
			os.Exit(1)
		}

		validateOrExit(nil, "drift cannot be checked")
		p, err := plan.ForState(true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR: unable to read the state files:", err)
			os.Exit(1)
		}
		actual, expected, err := db.ReadSchemas(p.Units[0].WithoutPostSql())
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		report.Findings = drift.Compare(expected, actual)
		if err = report.Ignore(append(viper.GetStringSlice("drift.ignore"), driftIgnore...)); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}

		if output == "json" {
			jsonOutput, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				fmt.Fprintln(os.Stderr, "Unable to marshal the drift report into JSON", err)
				os.Exit(1)
			}
			fmt.Println(string(jsonOutput))
		} else {
			drift.Render(os.Stdout, report)
		}
		if detailedExitcode && report.Drifted() {
			os.Exit(2)
		}
	},
}

func init() {
	rootCmd.AddCommand(driftCmd)
	driftCmd.Flags().StringSliceVar(&driftIgnore, "ignore", nil, "Leave out differences in objects matching this pattern (e.g. 'legacy_*' or 'documents.*_idx') - repeatable, added to drift.ignore in the config")
	driftCmd.Flags().BoolVar(&detailedExitcode, "detailed-exitcode", false, "Exit with 2 when there is drift (0 when there is none, 1 on an error)")
	driftCmd.Flags().BoolVar(&skipValidation, "skip-validation", false, "Compare without first validating the state files against the skima JSON schemas")
}
