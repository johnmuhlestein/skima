package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"skima/pkg/db"
	"skima/pkg/manifest"
	"skima/pkg/plan"
	"skima/pkg/util"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var planFrom string
var showScripts bool
var detailedExitcode bool

// a version as --from accepts it: 1.4.2, v1_4_2, 1.4.2-rc.1
var versionPattern = regexp.MustCompile(`^v?\d+[._]\d+[._]\d+(-[A-Za-z]+([._]\d+)?)?$`)

// planCmd represents the plan command
var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Shows what skima apply would run, without changing the database.",
	Long: `Lists every statement skima apply would run against the schema, in the order it runs them, without applying
anything - the same as skima apply --dry-run.

By default plan connects to the database to read the deployed version from the history tables. It only reads: it does
not take the apply lock and records no history. If skima does not manage the schema yet, the plan is the state build.

Use --from to plan without a database connection:
  --from <version>  plan the changesets newer than this version, e.g. the version deployed to production
  --from none       plan the state build of a new schema

Like apply, plan first validates the state files and the changesets it would apply (see skima manifest validate),
and fails if any is invalid - use --skip-validation to plan anyway.

Each step is preceded by a comment with its delta number in the changeset, what it does and the postgres error codes
apply treats as success for it. Steps that run a sql file name the file - use --show-scripts to print its contents.
Use the global --output=json flag for a machine readable plan, which always includes the sql of every step.

The exit code is 0 when the plan was made and 1 on an error. With --detailed-exitcode it is 2 when there are changes
to apply, which lets a CI job tell "up to date" from "changes pending".

Example:
  skima plan
  skima plan --from 1.4.2
  skima plan --from none --show-scripts
  skima plan --from 1.4.2 --output=json
  skima apply --dry-run`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		runPlan(planFrom)
	},
}

func init() {
	rootCmd.AddCommand(planCmd)
	planCmd.Flags().StringVar(&planFrom, "from", "", "Plan from this deployed version without connecting to the database - 'none' plans the state build of a new schema")
	planCmd.Flags().BoolVar(&showScripts, "show-scripts", false, "Print the contents of the sql files the plan runs, not just their names")
	planCmd.Flags().BoolVar(&detailedExitcode, "detailed-exitcode", false, "Exit with 2 when there are changes to apply (0 when there are none, 1 on an error)")
	planCmd.Flags().BoolVar(&skipValidation, "skip-validation", false, "Plan without first validating the manifest files against the skima JSON schemas")
}

// runPlan plans the apply from the given version - read from the database when from is empty - and prints the plan
func runPlan(from string) {
	var p plan.Plan
	var err error
	deployed, managed := deployedVersion(from)
	if managed {
		planValidation(pendingChangesets(deployed))
		p, err = plan.ForChangesets(deployed)
	} else {
		planValidation(nil)
		p, err = plan.ForState(viper.GetBool("state.transaction"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: unable to plan the apply:", err)
		os.Exit(1)
	}

	if output == "json" {
		jsonOutput, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to marshal the plan into JSON", err)
			os.Exit(1)
		}
		fmt.Println(string(jsonOutput))
	} else {
		plan.Render(os.Stdout, p, plan.RenderOptions{ShowScripts: showScripts})
	}
	if detailedExitcode && p.Pending() {
		os.Exit(2)
	}
}

// deployedVersion returns the version to plan from and whether skima manages the schema - when it does not, apply
// builds the schema from the state files. Without --from the version is read from the database's history tables
func deployedVersion(from string) (util.SemanticVersion, bool) {
	switch {
	case from == "none":
		return util.SemanticVersion{}, false
	case from != "":
		if !versionPattern.MatchString(from) {
			fmt.Fprintf(os.Stderr, "--from must be a version such as 1.4.2 or 1.4.2-rc.1, or 'none' for a new schema - got %q\n", from)
			os.Exit(1)
		}
		return util.ExtractVersion(from), true
	}
	hist, err := db.GetApplyHistory()
	switch err {
	case nil:
		return hist.SchemaVersion, true
	case db.NoHistTable, pgx.ErrNoRows:
		return util.SemanticVersion{}, false
	default:
		fmt.Fprintln(os.Stderr, "Unable to retrieve the apply history:", err)
		os.Exit(1)
	}
	return util.SemanticVersion{}, false
}

// planValidation validates what apply would validate - the state files and the given changesets - and exits if any
// is invalid, since apply would stop there
func planValidation(changesets []string) {
	if skipValidation {
		return
	}
	results, err := manifest.ValidateForApply(viper.GetString("workdir"), changesets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Unable to read the manifest files:", err)
		os.Exit(1)
	}
	for _, r := range results {
		if !r.Valid {
			reportValidation(results, false, false)
			fmt.Fprintln(os.Stderr, "ERROR: the manifest is invalid - skima apply would not apply anything. Fix the files above, or rerun with --skip-validation")
			os.Exit(1)
		}
	}
}
