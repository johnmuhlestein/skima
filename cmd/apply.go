package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"skima/pkg/db"
	"skima/pkg/manifest"
	"skima/pkg/plan"
	"skima/pkg/util"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/viper"

	"github.com/spf13/cobra"
)

var bootstrap bool
var hist bool
var baseline bool
var baselineVersion string
var skipValidation bool
var statesql bool
var dryRun bool
var activeApplyHist db.ApplyHistory
var manifestVersion util.SemanticVersion

// applyCmd represents the apply command
var applyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Applies schema to the database.",
	Long: `Creates database resources in the designated database.

Without designating any additional flags, the behavior is to apply the schema defined in the referenced workdir configuration
This means checking the skima_schema_history table to see if:
  1. Has schema previously been applied and if not apply the initial schema based on the state files
    a. At this time, the version of the schema will be either the version represented by the most recent changeset or "1.0.0" if there are no changesets.
    b. The state build (pre sql files, tables, foreign keys, views and post sql files) runs in a single transaction, so a failure
       rolls the whole build back and leaves no objects behind. If a state sql file cannot run inside a transaction (e.g.
       create index concurrently), set --state-transaction=false (state.transaction in the config, SKM_STATE_TRANSACTION).
  3. If the skima_schema_history tables indicate that there has been a successful application of a version of the schema
     then if there are changeset (delta) file(s) with a more recent version, those deltas will be applied in order.
     Each changeset is applied in a single transaction, so a failure rolls the whole changeset back (a changeset can
     opt out with "transaction": false).

Only one apply runs against a schema at a time - a second apply waits for the first to finish, for up to
--lock-timeout (5m by default; lock.timeout in the config, SKM_LOCK_TIMEOUT). If the first apply is still running after
that, the second exits without applying anything and reports which session holds the lock.

Before anything is applied, the state files and the pending changesets are validated against the skima JSON schemas
(see skima manifest validate --help). If any is invalid nothing is applied - use --skip-validation to bypass the check.

Use --dry-run to see what would be applied without changing anything - the same as skima plan (see skima plan --help).

This behavior assumes that there is a defined schema, a user has been bound to that schema as the default search path and that the skima 
schema management tables exist.

You can bootstrap a new schema using the --bootstrap flag.
Using the --bootstrap flag requires --dbsuperuser and --dbsuperuserpwd flags to be set, along with the schema
name (--dbschema) and the password for the new user (--dbpassword). User and schema names must be lowercase
letters, digits, _ or $. Re-running a bootstrap is safe.
This does the following:
  1. creates a new user (technically a role with connect privileges)
    a. if --dbusername is passed in, it will use that value, else the schema name will also be the username.
    b. the user is given the password passed in via --dbpassword or the SKM_DBCONN_PASSWORD environment variable.
       If the user already exists it is left as is, including its password.
  2. creates a new schema based on the value passed in via --dbschema or the SKM_DBCONN_SCHEMA environment variable.
  3. grants all privileges on the objects in the schema to the user, including grant rights
  4. sets the search path for the user to ONLY the new schema
`,
	Run: func(cmd *cobra.Command, args []string) {
		if dryRun {
			runPlan("")
			return
		}
		manifestVersion = manifest.Version()
		var failures int
		if bootstrap {
			if len(viper.GetString("dbconn.superuser")) == 0 || len(viper.GetString("dbconn.superuserpwd")) == 0 {
				fmt.Fprintln(os.Stderr, "--bootstrap requires the superuser credentials - set --dbsuperuser and --dbsuperuserpwd (or SKM_DBCONN_SUPERUSER and SKM_DBCONN_SUPERUSERPWD)")
				os.Exit(1)
			}
			if len(viper.GetString("dbconn.schema")) == 0 {
				fmt.Fprintln(os.Stderr, "--bootstrap requires a schema name - set --dbschema or SKM_DBCONN_SCHEMA")
				os.Exit(1)
			}
			if len(viper.GetString("dbconn.username")) == 0 {
				// the user defaults to the schema name - set before connecting so the history tables use it too
				viper.Set("dbconn.username", viper.GetString("dbconn.schema"))
			}
			db.Bootstrap(viper.GetString("dbconn.superuser"), viper.GetString("dbconn.superuserpwd"))
			db.LockApply(viper.GetDuration("lock.timeout"))
			db.CreateHistTables()
			return
		}
		// only one apply runs against a schema at a time - a concurrent apply waits here
		db.LockApply(viper.GetDuration("lock.timeout"))
		if hist {
			db.CreateHistTables()
		} else if baseline {
			db.CreateHistTables()
			version := util.ExtractVersion(baselineVersion)
			activeApplyHist = db.InitializeHistory(version, "baseline", "")
			activeApplyHist.Status = "success"
			db.FinalizeHistory(&activeApplyHist)
		} else if statesql {
			p := mustPlan(plan.ForStateSql(viper.GetBool("state.transaction")))
			activeApplyHist = db.InitializeHistory(manifestVersion, "sql", "")
			failures = applyUnit(p.Units[0])
		} else {
			hist, err := db.GetApplyHistory()
			switch err {
			case nil:
				fmt.Printf("Version %s is currently deployed - checking for a more current version to deploy\n", hist.SchemaVersion.String())
				// changesets take their definitions from the state files, so those are validated too
				validateBeforeApply(pendingChangesets(hist.SchemaVersion))
				failures = executeChangesets(hist.SchemaVersion)
			case db.NoHistTable:
				validateBeforeApply(nil)
				p := mustPlan(plan.ForState(viper.GetBool("state.transaction")))
				fmt.Println("Schema is not yet managed by skima - creating the history tables")
				db.CreateHistTables()
				fmt.Println("Applying the state definitions")
				failures = executeState(p)
			case pgx.ErrNoRows:
				validateBeforeApply(nil)
				p := mustPlan(plan.ForState(viper.GetBool("state.transaction")))
				fmt.Println("No objects currently managed - will apply the state definition")
				failures = executeState(p)
			default:
				fmt.Printf("unable to retrieve apply history: %s", err)
				os.Exit(1)
			}
			hist, err = db.GetApplyHistory()
			switch err {
			case nil:
				fmt.Printf("Last successfully completed version: %s on %s\n", hist.SchemaVersion.String(), hist.CompletionTimestamp.String())
			case pgx.ErrNoRows:
				fmt.Println("No successful builds found")
			default:
				fmt.Fprintf(os.Stderr, "unable to retrieve apply history: %s\n", err)
			}
		}
		// exit codes are 0-255, so a raw failure count of 256 would report success
		if failures > 0 {
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(applyCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	applyCmd.Flags().BoolVar(&bootstrap, "bootstrap", false, "Only run a db bootstrap")
	applyCmd.Flags().BoolVar(&hist, "hist", false, "Only apply the history tables to an existing schema")
	applyCmd.Flags().BoolVar(&baseline, "baseline", false, "Takes an existing schema that is not managed by skima and sets up the history tables and sets an initial")
	applyCmd.Flags().StringVar(&baselineVersion, "baseline-version", "1.0.0", "The version to apply to the baseline - 1.0.0 by default")
	applyCmd.MarkFlagsMutuallyExclusive("bootstrap", "hist", "baseline")
	applyCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be applied without changing the database - the same as skima plan")
	applyCmd.Flags().BoolVar(&skipValidation, "skip-validation", false, "Apply without first validating the manifest files against the skima JSON schemas")
	applyCmd.Flags().BoolVar(&statesql, "sql", false, "Only apply the sql files that are part of the state definition")
	applyCmd.Flags().Bool("state-transaction", true, "Apply the state build (and --sql) in a single transaction - set to false when a state sql file cannot run inside a transaction - overrides the config file value")
	viper.BindPFlag("state.transaction", applyCmd.Flags().Lookup("state-transaction"))
	applyCmd.Flags().Duration("lock-timeout", 5*time.Minute, "How long to wait for another apply against the schema to finish (e.g. 30s, 15m) - 0 waits indefinitely - overrides the config file value")
	viper.BindPFlag("lock.timeout", applyCmd.Flags().Lookup("lock-timeout"))
	for _, flag := range []string{"bootstrap", "hist", "baseline", "sql"} {
		applyCmd.MarkFlagsMutuallyExclusive("dry-run", flag)
	}
}

// pendingChangesetFiles returns the changeset files newer than the deployed version, in the order they are applied
func pendingChangesetFiles(deployed util.SemanticVersion) ([]string, error) {
	deltaFileNames, err := manifest.ListFiles(filepath.Join(viper.GetString("workdir"), "deltas"), "json")
	if err != nil {
		return nil, err
	}
	return util.NewerVersions(deployed, deltaFileNames), nil
}

func pendingChangesets(deployed util.SemanticVersion) []string {
	changes, err := pendingChangesetFiles(deployed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Unable to read the changeset files:", err)
		os.Exit(1)
	}
	return changes
}

// validateBeforeApply validates the state files and the given changesets, exiting before anything is applied if any
// of them is invalid
func validateBeforeApply(changesets []string) {
	if skipValidation {
		fmt.Println("Skipping manifest validation (--skip-validation)")
		return
	}
	results, err := manifest.ValidateForApply(viper.GetString("workdir"), changesets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Unable to read the manifest files:", err)
		os.Exit(1)
	}
	fmt.Println("Validating the manifest files")
	if invalid := reportValidation(results, false, false); invalid > 0 {
		fmt.Fprintln(os.Stderr, "ERROR: the manifest is invalid - nothing was applied. Fix the files above, or rerun with --skip-validation")
		os.Exit(1)
	}
}

// mustPlan returns the plan, exiting if it could not be made - before anything has been applied
func mustPlan(p plan.Plan, err error) plan.Plan {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: unable to plan the apply - nothing was applied:", err)
		os.Exit(1)
	}
	return p
}

// executeChangesets plans the changesets newer than the deployed version and applies them in order, each as its own
// run, stopping at the first that fails. It returns the number of failures
func executeChangesets(deployed util.SemanticVersion) int {
	p := mustPlan(plan.ForChangesets(deployed))
	if !p.Pending() {
		fmt.Printf("There are no changes more current than %s - nothing to see here\n", deployed.String())
		return 0
	}
	fmt.Printf("%d new changesets to apply\n", len(p.Units))
	for _, unit := range p.Units {
		activeApplyHist = db.InitializeHistory(unit.SemVer, "changeset", filepath.Base(unit.File))
		fmt.Printf("Initialized run %d for version %s\n", activeApplyHist.RunId, unit.Version)
		if failures := applyUnit(unit); failures > 0 {
			return failures
		}
	}
	return 0
}

// executeState applies the planned state build. Unless state.transaction is false the whole build runs in a single
// transaction, so a failure rolls back every object it created
func executeState(p plan.Plan) int {
	activeApplyHist = db.InitializeHistory(p.Units[0].SemVer, "state", "")
	fmt.Printf("Executing state files as run: %d\n", activeApplyHist.RunId)
	return applyUnit(p.Units[0])
}

// applyUnit applies a planned run as the active history run, then reports and records its outcome. It returns the
// number of failures
func applyUnit(unit plan.Unit) int {
	failures := db.ApplyUnit(&activeApplyHist, unit)
	if failures > 0 {
		// a failed changeset keeps the status the apply gave it (e.g. fast-fail) - the state build is marked failed
		if unit.Kind != plan.UnitChangeset && activeApplyHist.Status != "rolled back" {
			activeApplyHist.Status = "failed"
		}
		fmt.Fprintf(os.Stderr, "ERROR: RunID %d had %d failures applying objects which could indicate additional downstream failures. Review the History to determine failures\n", activeApplyHist.RunId, failures)
	} else {
		fmt.Printf("SUCCESS: Run %d completed without failures\n", activeApplyHist.RunId)
		activeApplyHist.Status = "success"
	}
	db.FinalizeHistory(&activeApplyHist)
	return failures
}
