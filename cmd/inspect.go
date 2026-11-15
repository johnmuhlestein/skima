package cmd

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/johnmuhlestein/skima/internal/db"
	"github.com/spf13/cobra"
)

func newInspectCmd(root *rootOptions) *cobra.Command {
	var hist bool
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Inspects the schema history recorded in the database.",
		Long: `Reads the schema management history tables in the target database and shows the most recent successful run:
the schema version, apply type, start/completion times and each statement that was applied as part of that run.
If no run has completed successfully, "No successful builds found" is reported. The exit code is 1 when the history
can't be read.

Use the global --output=json flag to get the history as JSON instead of plain text.

Example:
  skima inspect
  skima inspect --output=json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			applyHist, err := db.GetApplyHistory(cmd.Context())
			switch {
			case errors.Is(err, db.ErrNoHistTable):
				fmt.Fprintln(out, "Schema is not managed by skima - run 'skima apply' to deploy it, or 'skima apply --hist' to only create the history tables")
			case errors.Is(err, pgx.ErrNoRows):
				fmt.Fprintln(out, "No successful builds found")
			case err != nil:
				return fmt.Errorf("unable to read the history: %w", err)
			case root.json():
				jsonOutput, err := json.Marshal(applyHist)
				if err != nil {
					return fmt.Errorf("unable to marshal the history into JSON: %w", err)
				}
				fmt.Fprintln(out, string(jsonOutput))
			default:
				fmt.Fprintln(out, applyHist.String())
			}
			return nil
		},
	}
	// the history is what inspect shows - --hist is kept so existing scripts keep working
	cmd.Flags().BoolVar(&hist, "hist", false, "Show the most recent successful run (the default)")
	cobra.CheckErr(cmd.Flags().MarkHidden("hist"))
	return cmd
}
