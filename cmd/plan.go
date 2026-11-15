package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/johnmuhlestein/skima/internal/db"
	"github.com/johnmuhlestein/skima/internal/manifest"
	"github.com/johnmuhlestein/skima/internal/plan"
	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// a version as --from accepts it: 1.4.2, v1_4_2, 1.4.2-rc.1
var versionPattern = regexp.MustCompile(`^v?\d+[._]\d+[._]\d+(-[A-Za-z]+([._]\d+)?)?$`)

// planOptions are plan's flags - apply --dry-run plans with its own skip-validation
type planOptions struct {
	from             string
	showScripts      bool
	detailedExitcode bool
	skipValidation   bool
}

func newPlanCmd(root *rootOptions) *cobra.Command {
	opts := &planOptions{}
	cmd := &cobra.Command{
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(cmd, root, opts)
		},
	}
	cmd.Flags().StringVar(&opts.from, "from", "", "Plan from this deployed version without connecting to the database - 'none' plans the state build of a new schema")
	cmd.Flags().BoolVar(&opts.showScripts, "show-scripts", false, "Print the contents of the sql files the plan runs, not just their names")
	cmd.Flags().BoolVar(&opts.detailedExitcode, "detailed-exitcode", false, "Exit with 2 when there are changes to apply (0 when there are none, 1 on an error)")
	cmd.Flags().BoolVar(&opts.skipValidation, "skip-validation", false, "Plan without first validating the manifest files against the skima JSON schemas")
	return cmd
}

// runPlan plans the apply from the given version - read from the database when from is empty - and prints the plan
func runPlan(cmd *cobra.Command, root *rootOptions, opts *planOptions) error {
	deployed, managed, err := deployedVersion(cmd.Context(), opts.from)
	if err != nil {
		return err
	}
	var p plan.Plan
	if managed {
		pending, err := pendingChangesets(deployed)
		if err != nil {
			return err
		}
		if err = validateFor(cmd, opts.skipValidation, pending, "skima apply would not apply anything", false); err != nil {
			return err
		}
		p, err = planner().ForChangesets(deployed)
		if err != nil {
			return fmt.Errorf("unable to plan the apply: %w", err)
		}
	} else {
		if err = validateFor(cmd, opts.skipValidation, nil, "skima apply would not apply anything", false); err != nil {
			return err
		}
		if p, err = planner().ForState(viper.GetBool("state.transaction")); err != nil {
			return fmt.Errorf("unable to plan the apply: %w", err)
		}
	}

	if root.json() {
		jsonOutput, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return fmt.Errorf("unable to marshal the plan into JSON: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(jsonOutput))
	} else {
		plan.Render(cmd.OutOrStdout(), p, plan.RenderOptions{ShowScripts: opts.showScripts})
	}
	if opts.detailedExitcode && p.Pending() {
		return exitCode(2)
	}
	return nil
}

// deployedVersion returns the version to plan from and whether skima manages the schema - when it does not, apply
// builds the schema from the state files. Without --from the version is read from the database's history tables
func deployedVersion(ctx context.Context, from string) (util.SemanticVersion, bool, error) {
	switch {
	case from == "none":
		return util.SemanticVersion{}, false, nil
	case from != "":
		if !versionPattern.MatchString(from) {
			return util.SemanticVersion{}, false, fmt.Errorf("--from must be a version such as 1.4.2 or 1.4.2-rc.1, or 'none' for a new schema - got %q", from)
		}
		return util.ExtractVersion(from), true, nil
	}
	database, err := openDB()
	if err != nil {
		return util.SemanticVersion{}, false, err
	}
	defer database.Close()
	hist, err := database.GetApplyHistory(ctx)
	switch {
	case err == nil:
		return hist.SchemaVersion, true, nil
	case errors.Is(err, db.ErrNoHistTable), errors.Is(err, pgx.ErrNoRows):
		return util.SemanticVersion{}, false, nil
	default:
		return util.SemanticVersion{}, false, fmt.Errorf("unable to retrieve the apply history: %w", err)
	}
}

// validateFor validates what apply would validate - the state files and the given changesets - and returns an error
// if any is invalid, after listing the problems. consequence says what that means, e.g. that apply would stop there.
// announce says so before validating, as apply does
func validateFor(cmd *cobra.Command, skip bool, changesets []string, consequence string, announce bool) error {
	if skip {
		if announce {
			fmt.Fprintln(cmd.OutOrStdout(), "Skipping manifest validation (--skip-validation)")
		}
		return nil
	}
	results, err := manifest.ValidateForApply(viper.GetString("workdir"), changesets)
	if err != nil {
		return fmt.Errorf("unable to read the manifest files: %w", err)
	}
	if announce {
		fmt.Fprintln(cmd.OutOrStdout(), "Validating the manifest files")
	}
	for _, r := range results {
		if !r.Valid {
			reportValidation(cmd, results, false, false)
			return fmt.Errorf("the manifest is invalid - %s. Fix the files above, or rerun with --skip-validation", consequence)
		}
	}
	return nil
}

// pendingChangesets returns the changeset files newer than the deployed version, in the order they are applied
func pendingChangesets(deployed util.SemanticVersion) ([]string, error) {
	files, err := manifest.ListFiles(filepath.Join(viper.GetString("workdir"), "deltas"), "json")
	if err != nil {
		return nil, fmt.Errorf("unable to read the changeset files: %w", err)
	}
	return util.NewerVersions(deployed, files), nil
}
