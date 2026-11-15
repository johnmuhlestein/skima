package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/johnmuhlestein/skima/internal/db"
	"github.com/johnmuhlestein/skima/internal/manifest"
	"github.com/johnmuhlestein/skima/internal/plan"
	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// applyOptions are apply's flags
type applyOptions struct {
	bootstrap       bool
	hist            bool
	baseline        bool
	baselineVersion string
	skipValidation  bool
	stateSql        bool
	dryRun          bool
}

func newApplyCmd(root *rootOptions) *cobra.Command {
	opts := &applyOptions{}
	cmd := &cobra.Command{
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
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.dryRun {
				return runPlan(cmd, root, &planOptions{skipValidation: opts.skipValidation})
			}
			a := &applier{ctx: cmd.Context(), planner: planner(), cmd: cmd, out: cmd.OutOrStdout(), errOut: cmd.ErrOrStderr(), skipValidation: opts.skipValidation}
			return a.apply(opts)
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&opts.bootstrap, "bootstrap", false, "Only run a db bootstrap")
	flags.BoolVar(&opts.hist, "hist", false, "Only apply the history tables to an existing schema")
	flags.BoolVar(&opts.baseline, "baseline", false, "Adopt an existing schema that skima does not manage yet: create the history tables and record --baseline-version as deployed, without applying anything")
	flags.StringVar(&opts.baselineVersion, "baseline-version", "1.0.0", "The version to apply to the baseline - 1.0.0 by default")
	cmd.MarkFlagsMutuallyExclusive("bootstrap", "hist", "baseline")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "Show what would be applied without changing the database - the same as skima plan")
	flags.BoolVar(&opts.skipValidation, "skip-validation", false, "Apply without first validating the manifest files against the skima JSON schemas")
	flags.BoolVar(&opts.stateSql, "sql", false, "Only apply the sql files that are part of the state definition")
	flags.Bool("state-transaction", true, "Apply the state build (and --sql) in a single transaction - set to false when a state sql file cannot run inside a transaction - overrides the config file value")
	cobra.CheckErr(viper.BindPFlag("state.transaction", flags.Lookup("state-transaction")))
	flags.Duration("lock-timeout", 5*time.Minute, "How long to wait for another apply against the schema to finish (e.g. 30s, 15m) - 0 waits indefinitely - overrides the config file value")
	cobra.CheckErr(viper.BindPFlag("lock.timeout", flags.Lookup("lock-timeout")))
	for _, flag := range []string{"bootstrap", "hist", "baseline", "sql"} {
		cmd.MarkFlagsMutuallyExclusive("dry-run", flag)
	}
	return cmd
}

// applier runs one skima apply: the runs it applies and records
type applier struct {
	ctx            context.Context
	db             *db.DB
	planner        plan.Planner
	cmd            *cobra.Command
	out, errOut    io.Writer
	skipValidation bool
	// failures counts the failed statements of the runs applied
	failures int
}

// errApplyFailed ends an apply that recorded failures - they were reported as they happened
var errApplyFailed = exitCode(1)

func (a *applier) apply(opts *applyOptions) error {
	ctx := a.ctx
	if opts.bootstrap {
		if viper.GetString("dbconn.superuser") == "" || viper.GetString("dbconn.superuserpwd") == "" {
			return errors.New("--bootstrap requires the superuser credentials - set SKM_DBCONN_SUPERUSER and SKM_DBCONN_SUPERUSERPWD (or --dbsuperuser and --dbsuperuserpwd)")
		}
		if viper.GetString("dbconn.schema") == "" {
			return errors.New("--bootstrap requires a schema name - set --dbschema or SKM_DBCONN_SCHEMA")
		}
		if viper.GetString("dbconn.username") == "" {
			// the user defaults to the schema name - set before connecting so the history tables use it too
			viper.Set("dbconn.username", viper.GetString("dbconn.schema"))
		}
		if err := db.Bootstrap(ctx, dbConfig(), viper.GetString("dbconn.superuser"), viper.GetString("dbconn.superuserpwd"), a.out); err != nil {
			return err
		}
		if err := a.open(); err != nil {
			return err
		}
		defer a.db.Close()
		unlock, err := a.db.LockApply(ctx, viper.GetDuration("lock.timeout"), a.out)
		if err != nil {
			return err
		}
		defer unlock()
		return a.db.CreateHistTables(ctx, a.out)
	}

	if err := a.open(); err != nil {
		return err
	}
	defer a.db.Close()
	// only one apply runs against a schema at a time - a concurrent apply waits here
	unlock, err := a.db.LockApply(ctx, viper.GetDuration("lock.timeout"), a.out)
	if err != nil {
		return err
	}
	defer unlock()
	switch {
	case opts.hist:
		return a.db.CreateHistTables(ctx, a.out)
	case opts.baseline:
		if err := a.db.CreateHistTables(ctx, a.out); err != nil {
			return err
		}
		run, err := a.db.InitializeHistory(ctx, util.ExtractVersion(opts.baselineVersion), "baseline", "")
		if err != nil {
			return err
		}
		run.Status = "success"
		return a.db.FinalizeHistory(ctx, &run)
	case opts.stateSql:
		p, err := a.planner.ForStateSql(viper.GetBool("state.transaction"))
		if err != nil {
			return planError(err)
		}
		if err = a.applyUnit(p.Units[0], manifest.Version(a.planner.Workdir), "sql", ""); err != nil {
			return err
		}
		return a.result()
	}

	hist, err := a.db.GetApplyHistory(ctx)
	switch {
	case err == nil:
		fmt.Fprintf(a.out, "Version %s is currently deployed - checking for a more current version to deploy\n", hist.SchemaVersion.String())
		pending, err := pendingChangesets(hist.SchemaVersion)
		if err != nil {
			return err
		}
		// changesets take their definitions from the state files, so those are validated too
		if err = a.validate(pending); err != nil {
			return err
		}
		if err = a.applyChangesets(hist.SchemaVersion); err != nil {
			return err
		}
	case errors.Is(err, db.ErrNoHistTable), errors.Is(err, pgx.ErrNoRows):
		noHistTables := errors.Is(err, db.ErrNoHistTable)
		if err := a.validate(nil); err != nil {
			return err
		}
		p, err := a.planner.ForState(viper.GetBool("state.transaction"))
		if err != nil {
			return planError(err)
		}
		if noHistTables {
			fmt.Fprintln(a.out, "Schema is not yet managed by skima - creating the history tables")
			if err := a.db.CreateHistTables(ctx, a.out); err != nil {
				return err
			}
			fmt.Fprintln(a.out, "Applying the state definitions")
		} else {
			fmt.Fprintln(a.out, "No objects currently managed - will apply the state definition")
		}
		if err = a.applyUnit(p.Units[0], p.Units[0].SemVer, "state", ""); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unable to retrieve the apply history: %w", err)
	}
	if ctx.Err() == nil {
		hist, err := a.db.GetApplyHistory(ctx)
		switch {
		case err == nil:
			fmt.Fprintf(a.out, "Last successfully completed version: %s on %s\n", hist.SchemaVersion.String(), hist.CompletionTimestamp.String())
		case errors.Is(err, pgx.ErrNoRows):
			fmt.Fprintln(a.out, "No successful builds found")
		default:
			fmt.Fprintf(a.errOut, "unable to retrieve the apply history: %s\n", err)
		}
	}
	return a.result()
}

// open opens the configured database
func (a *applier) open() error {
	database, err := openDB()
	if err != nil {
		return err
	}
	a.db = database
	return nil
}

// result is the apply's outcome once its runs are applied - interrupted, failed or a success
func (a *applier) result() error {
	switch {
	case a.ctx.Err() != nil:
		return &exitError{code: exitInterrupted}
	case a.failures > 0:
		return errApplyFailed
	}
	return nil
}

// validate validates the state files and the given changesets, failing before anything is applied if any of them is
// invalid
func (a *applier) validate(changesets []string) error {
	return validateFor(a.cmd, a.skipValidation, changesets, "nothing was applied", true)
}

// planError reports a plan that could not be made - before anything has been applied
func planError(err error) error {
	return fmt.Errorf("unable to plan the apply - nothing was applied: %w", err)
}

// applyChangesets plans the changesets newer than the deployed version and applies them in order, each as its own
// run, stopping at the first that fails
func (a *applier) applyChangesets(deployed util.SemanticVersion) error {
	p, err := a.planner.ForChangesets(deployed)
	if err != nil {
		return planError(err)
	}
	if !p.Pending() {
		fmt.Fprintf(a.out, "There are no changes more current than %s - nothing to see here\n", deployed.String())
		return nil
	}
	fmt.Fprintf(a.out, "%d new changesets to apply\n", len(p.Units))
	for _, unit := range p.Units {
		if err := a.applyUnit(unit, unit.SemVer, "changeset", filepath.Base(unit.File)); err != nil {
			return err
		}
		if a.failures > 0 || a.ctx.Err() != nil {
			return nil
		}
	}
	return nil
}

// applyUnit records a run for the planned unit, applies it, then reports and records its outcome. An error means the
// run could not be recorded; the unit's failures are counted in a.failures
func (a *applier) applyUnit(unit plan.Unit, version util.SemanticVersion, applyType string, file string) error {
	run, err := a.db.InitializeHistory(a.ctx, version, applyType, file)
	if err != nil {
		return err
	}
	switch applyType {
	case "changeset":
		fmt.Fprintf(a.out, "Initialized run %d for version %s\n", run.RunId, unit.Version)
	case "state":
		fmt.Fprintf(a.out, "Executing state files as run: %d\n", run.RunId)
	}
	failures, err := a.db.ApplyUnit(a.ctx, &run, unit, a.out, a.errOut)
	if err != nil {
		fmt.Fprintln(a.errOut, "ERROR:", err)
	}
	switch {
	case run.Status == "cancelled":
		fmt.Fprintf(a.errOut, "ERROR: RunID %d was interrupted - nothing it applied in a transaction was kept\n", run.RunId)
	case failures > 0:
		// a failed changeset keeps the status the apply gave it (e.g. fast-fail) - the state build is marked failed
		if unit.Kind != plan.UnitChangeset && run.Status != "rolled back" {
			run.Status = "failed"
		}
		fmt.Fprintf(a.errOut, "ERROR: RunID %d had %d failures applying objects which could indicate additional downstream failures. Review the History to determine failures\n", run.RunId, failures)
	default:
		fmt.Fprintf(a.out, "SUCCESS: Run %d completed without failures\n", run.RunId)
		run.Status = "success"
	}
	a.failures += failures
	return a.db.FinalizeHistory(a.ctx, &run)
}
