package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"skima/pkg/db"
	"skima/pkg/manifest"
	"skima/pkg/util"

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

Only one apply runs against a schema at a time - a second apply waits for the first to finish.

Before anything is applied, the state files and the pending changesets are validated against the skima JSON schemas
(see skima manifest validate --help). If any is invalid nothing is applied - use --skip-validation to bypass the check.

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
			db.LockApply()
			db.CreateHistTables()
			return
		}
		// only one apply runs against a schema at a time - a concurrent apply waits here
		db.LockApply()
		if hist {
			db.CreateHistTables()
		} else if baseline {
			db.CreateHistTables()
			version := util.ExtractVersion(baselineVersion)
			activeApplyHist = db.InitializeHistory(version, "baseline", "")
			activeApplyHist.Status = "success"
			db.FinalizeHistory(&activeApplyHist)
		} else if statesql {
			activeApplyHist = db.InitializeHistory(manifestVersion, "sql", "")
			failures = 1
			if build := db.BeginStateBuild(&activeApplyHist, "state sql run", viper.GetBool("state.transaction")); build != nil {
				failures = build.Finish(executeSqlState(build, &activeApplyHist, "pre"))
			}
			if failures > 0 {
				if activeApplyHist.Status != "rolled back" {
					activeApplyHist.Status = "failed"
				}
				fmt.Fprintf(os.Stderr, "ERROR: RunID %d had %d failures applying objects which could indicate additional downstream failures. Review the History to determine failures\n", activeApplyHist.RunId, failures)
			} else {
				fmt.Printf("SUCCESS: Run %d completed without failures\n", activeApplyHist.RunId)
				activeApplyHist.Status = "success"
			}
			db.FinalizeHistory(&activeApplyHist)
		} else {
			hist, err := db.GetApplyHistory()
			switch err {
			case nil:
				fmt.Printf("Version %s is currently deployed - checking for a more current version to deploy\n", hist.SchemaVersion.String())
				// changesets take their definitions from the state files, so those are validated too
				validateBeforeApply(pendingChangesets(hist.SchemaVersion))
				failures = executeChangeset(hist.SchemaVersion)
			case db.NoHistTable:
				validateBeforeApply(nil)
				fmt.Println("Schema is not yet managed by skima - creating the history tables")
				db.CreateHistTables()
				fmt.Println("Applying the state definitions")
				failures = executeState()
			case pgx.ErrNoRows:
				validateBeforeApply(nil)
				fmt.Println("No objects currently managed - will apply the state definition")
				failures = executeState()
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
	applyCmd.Flags().BoolVar(&skipValidation, "skip-validation", false, "Apply without first validating the manifest files against the skima JSON schemas")
	applyCmd.Flags().BoolVar(&statesql, "sql", false, "Only apply the sql files that are part of the state definition")
	applyCmd.Flags().Bool("state-transaction", true, "Apply the state build (and --sql) in a single transaction - set to false when a state sql file cannot run inside a transaction - overrides the config file value")
	viper.BindPFlag("state.transaction", applyCmd.Flags().Lookup("state-transaction"))
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

// executeChangeset initializes apply history internally
// returns the number of errors encountered.
func executeChangeset(oldVersion util.SemanticVersion) int {
	var failures int
	if changes, err := pendingChangesetFiles(oldVersion); err == nil {
		if len(changes) > 0 {
			fmt.Printf("%d new changesets to apply\n", len(changes))
			for _, change := range changes {
				changeset, err := manifest.ParseChangeSet(change)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR parsing the changeset file %s: %s\n", change, err)
					os.Exit(1)
				}
				activeApplyHist = db.InitializeHistory(changeset.SemVer, "changeset", filepath.Base(change))
				fmt.Printf("Initialized run %d for version %s\n", activeApplyHist.RunId, changeset.Version)

				failures += db.ApplyChangeset(&activeApplyHist, changeset)
				if failures > 0 {
					fmt.Fprintf(os.Stderr, "ERROR: RunID %d had %d failures applying objects which could indicate additional downstream failures. Review the History to determine failures\n", activeApplyHist.RunId, failures)
				} else {
					fmt.Printf("SUCCESS: Run %d completed without failures\n", activeApplyHist.RunId)
					activeApplyHist.Status = "success"
				}
				db.FinalizeHistory(&activeApplyHist)
				if failures > 0 {
					return failures
				} else {
					// reset if there are additional files to process
					failures = 0
				}
			}
		} else {
			fmt.Printf("There are no changes more current than %s - nothing to see here\n", oldVersion.String())
		}
	}
	return failures
}

// executeSqlState applies the state sql files of the phase as part of the build, returning the number of failures
func executeSqlState(build *db.StateBuild, history *db.ApplyHistory, phase string) int {
	fmt.Println("Beginning to apply SQL state files")
	var failures int
	sqlFilePaths, err := manifest.ListFiles(filepath.Join(viper.GetString("workdir"), "state", "sql", phase), "sql")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error reading state sql file directory:", err)
		return 1
	}
	for _, sql := range sqlFilePaths {
		filename := filepath.Base(sql)
		dat, err := os.ReadFile(sql)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error reading state sql files:", sql, err)
			return failures + 1
		}
		ok := build.ApplySql(filename, string(dat))
		if !ok {
			failures++
			history.Status = "failed"
		}
	}
	return failures
}

// executeState will initialize apply history internally. Unless state.transaction is false the whole build runs in
// a single transaction, so a failure rolls back every object it created.
func executeState() int {
	activeApplyHist = db.InitializeHistory(manifestVersion, "state", "")
	failures := 1
	if build := db.BeginStateBuild(&activeApplyHist, "state build", viper.GetBool("state.transaction")); build != nil {
		failures = build.Finish(buildState(build))
	}

	if failures > 0 {
		if activeApplyHist.Status != "rolled back" {
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

// buildState applies the pre sql files, tables, foreign keys, views and post sql files, returning the number of
// failures. It stops early when a step fails that later steps depend on.
func buildState(build *db.StateBuild) int {
	// First apply sql - should not have table dependencies
	failures := executeSqlState(build, &activeApplyHist, "pre")
	if failures > 0 {
		return failures
	}
	// Start applying tables
	tableFilePaths, err := filepath.Glob(filepath.Join(viper.GetString("workdir"), "state", "*-table.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Malformed filter for table paths:", err)
		return failures + 1
	}
	fmt.Printf("Executing state files as run: %d\n", activeApplyHist.RunId)
	fks := make([]manifest.Table, 0)
	for _, path := range tableFilePaths {
		table, err := manifest.ParseTableByPath(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error reading state files:", path, err)
			return failures + 1
		}
		ok := build.ApplyTable(table)
		if !ok {
			failures++
		} else if len(table.ForeignKeys) > 0 {
			fks = append(fks, table)
		}
	}
	// Now apply the foreign keys - we do this as a second loop to make sure all tables have been added
	for _, tbl := range fks {
		ok := build.ApplyForeignKeys(tbl)
		if !ok {
			failures++
		}
	}
	// Start applying views
	viewFilePaths, err := filepath.Glob(filepath.Join(viper.GetString("workdir"), "state", "*-view.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Malformed filter for view paths:", err)
		failures++
	} else {
		var viewItr = 5
		var viewRetry []manifest.View
		for _, path := range viewFilePaths {
			view, err := manifest.ParseViewByPath(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error reading view state file:", path, err)
				return failures + 1
			}
			// only views missing a relation are retried - any other failure will not be fixed by trying again
			ok, retryable := build.ApplyView(view)
			if !ok && retryable {
				viewRetry = append(viewRetry, view)
			} else if !ok {
				failures++
			}
		}
		for i := 1; i <= viewItr; i++ {
			if len(viewRetry) > 0 {
				fmt.Printf("Retrying %d views\n", len(viewRetry))
				var viewRetry2 []manifest.View
				for _, view := range viewRetry {
					ok, retryable := build.ApplyView(view)
					if !ok && retryable {
						viewRetry2 = append(viewRetry2, view)
					} else if !ok {
						failures++
					}
				}
				// no view was created this pass, so nothing changed that could make another pass succeed
				noProgress := len(viewRetry2) == len(viewRetry)
				viewRetry = viewRetry2
				if noProgress {
					break
				}
			} else {
				viewRetry = nil
				break
			}
		}
		if len(viewRetry) > 0 {
			failures += len(viewRetry)
		}
	}

	// Finally apply post state sql - this is for loading data and items with table dependencies
	failures += executeSqlState(build, &activeApplyHist, "post")
	return failures
}
