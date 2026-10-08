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
  3. If the skima_schema_history tables indicate that there has been a successful application of a version of the schema
     then if there are changeset (delta) file(s) with a more recent version, those deltas will be applied in order.

This behavior assumes that there is a defined schema, a user has been bound to that schema as the default search path and that the skima 
schema management tables exist.

You can bootstrap a new schema using the --bootstrap flag.
Using the --bootstrap flag requires --dbsuperuser and --dbsuperuserpwd flags to be set.
This does the following:
  1. creates a new user (technically a role with connect privileges)
    a. if --dbusername is passed in, it will use that value, else the schema name will also be the username.
    b. if --dbpassword is passed in, that will be the password assigned to the user if not, a password will be generated.
  2. creates a new schema based on the value passed in via --dbschema or the PSM_DBCONN_SCHEMA environment variable.
  3. grants all privileges on the objects in the schema to the user, including grant rights
  4. sets the search path for the user to ONLY the new schema
`,
	Run: func(cmd *cobra.Command, args []string) {
		manifestVersion = manifest.Version()
		var failures int
		if bootstrap {
			if len(viper.GetString("dbconn.superuser")) > 0 {
				db.Bootstrap(viper.GetString("dbconn.superuser"), viper.GetString("dbconn.superuserpwd"))
				db.CreateHistTables()
			}
		} else if hist {
			db.CreateHistTables()
		} else if baseline {
			db.CreateHistTables()
			version := util.ExtractVersion(baselineVersion)
			activeApplyHist = db.InitializeHistory(version, "baseline", "")
			activeApplyHist.Status = "success"
			db.FinalizeHistory(&activeApplyHist)
		} else if statesql {
			activeApplyHist = db.InitializeHistory(manifestVersion, "sql", "")
			failures = executeSqlState(&activeApplyHist, "pre")
			if failures > 0 {
				activeApplyHist.Status = "failed"
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
				failures = executeChangeset(hist.SchemaVersion)
			case db.NoHistTable:
				fmt.Println("Schema is not yet managed by skima - creating the history tables")
				db.CreateHistTables()
				fmt.Println("Applying the state definitions")
				failures = executeState()
			case pgx.ErrNoRows:
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
		os.Exit(failures)
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
	applyCmd.Flags().BoolVar(&statesql, "sql", false, "Only apply the sql files that are part of the state definition")
}

// executeChangeset initializes apply history internally
// returns the number of errors encountered.
func executeChangeset(oldVersion util.SemanticVersion) int {
	deltaFileNames, err := manifest.ListFiles(filepath.Join(viper.GetString("workdir"), "deltas"), "json")
	var failures int
	if err == nil {
		changes := util.NewerVersions(oldVersion, deltaFileNames)
		if len(changes) > 0 {
			fmt.Printf("%d new changesets to apply\n", len(changes))
			for _, change := range changes {
				changeset, err := manifest.ParseChangeSet(change)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR parsing the changest file %s: %s\n", change, err)
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

// executeSqlState expects apply history to be initialized externally
func executeSqlState(history *db.ApplyHistory, phase string) int {
	fmt.Println("Beginning to apply SQL state files")
	var failures int
	sqlFilePaths, err := manifest.ListFiles(filepath.Join(viper.GetString("workdir"), "state", "sql", phase), "sql")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error reading state sql file directory:", err)
		os.Exit(1)
	}
	for _, sql := range sqlFilePaths {
		filename := filepath.Base(sql)
		dat, err2 := os.ReadFile(sql)
		if err2 != nil {
			fmt.Fprintln(os.Stderr, "Error reading state sql files:", sql, err)
			os.Exit(1)
		}
		ok := db.ApplySql(history, filename, string(dat))
		if !ok {
			failures++
			history.Status = "failed"
		}
	}
	return failures
}

// executeState will initialize apply history internally
func executeState() int {
	activeApplyHist = db.InitializeHistory(manifestVersion, "state", "")
	var failures int

	// First apply sql - should not have table dependencies
	failures = executeSqlState(&activeApplyHist, "pre")
	if failures < 1 {
		// Start applying tables
		tableFilePaths, err := filepath.Glob(filepath.Join(viper.GetString("workdir"), "state", "*-table.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Malformed filter for table paths:", err)
			os.Exit(1)
		}
		fmt.Printf("Executing state files as run: %d\n", activeApplyHist.RunId)
		fks := make([]manifest.Table, 0)
		for _, path := range tableFilePaths {
			table, err := manifest.ParseTableByPath(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error reading state files:", path, err)
				os.Exit(1)
			}
			ok := db.ApplyTable(&activeApplyHist, table)
			if !ok {
				failures++
			} else {
				if table.ForeignKeys != nil && len(table.ForeignKeys) > 0 {
					fks = append(fks, table)
				}
			}
		}
		// Now apply the foreign keys - we do this as a second loop to make sure all tables have been added
		for _, tbl := range fks {
			ok := db.ApplyForeignKeys(&activeApplyHist, tbl)
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
					os.Exit(1)
				}
				// only views missing a relation are retried - any other failure will not be fixed by trying again
				ok, retryable := db.ApplyView(&activeApplyHist, view)
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
						ok, retryable := db.ApplyView(&activeApplyHist, view)
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
		failures += executeSqlState(&activeApplyHist, "post")

	}

	if failures > 0 {
		activeApplyHist.Status = "failed"
		fmt.Fprintf(os.Stderr, "ERROR: RunID %d had %d failures applying objects which could indicate additional downstream failures. Review the History to determine failures\n", activeApplyHist.RunId, failures)
	} else {
		fmt.Printf("SUCCESS: Run %d completed without failures\n", activeApplyHist.RunId)
		activeApplyHist.Status = "success"
	}
	db.FinalizeHistory(&activeApplyHist)
	return failures
}
