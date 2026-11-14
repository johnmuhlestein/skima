package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/johnmuhlestein/skima/internal/db"
	"github.com/spf13/cobra"
)

var inspectHist bool

// inspectCmd represents the inspect command
var inspectCmd = &cobra.Command{
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
	Run: func(cmd *cobra.Command, args []string) {
		retrieveHist()
	},
}

func init() {
	rootCmd.AddCommand(inspectCmd)
	// the history is what inspect shows - --hist is kept so existing scripts keep working
	inspectCmd.Flags().BoolVar(&inspectHist, "hist", false, "Show the most recent successful run (the default)")
	cobra.CheckErr(inspectCmd.Flags().MarkHidden("hist"))
}

func retrieveHist() {
	applyHist, err := db.GetApplyHistory()
	switch {
	case errors.Is(err, db.ErrNoHistTable):
		fmt.Println("Schema is not managed by skima - run 'skima apply' to deploy it, or 'skima apply --hist' to only create the history tables")
	case errors.Is(err, pgx.ErrNoRows):
		fmt.Println("No successful builds found")
	case err != nil:
		fmt.Fprintln(os.Stderr, "ERROR: unable to read the history:", err)
		os.Exit(1)
	case output == "json":
		jsonOutput, err := json.Marshal(applyHist)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR: unable to marshal the history into JSON:", err)
			os.Exit(1)
		}
		fmt.Println(string(jsonOutput))
	default:
		fmt.Println(applyHist.String())
	}
}
