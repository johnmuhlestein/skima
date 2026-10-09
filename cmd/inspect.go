package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"skima/pkg/db"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

var histVer string
var allHist bool

// inspectCmd represents the inspect command
var inspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Inspects the schema history recorded in the database.",
	Long: `Reads the schema management history tables in the target database.

Use --hist to show the most recent successful run: the schema version, apply type,
start/completion times and each statement that was applied as part of that run.
If no run has completed successfully, "No successful builds found" is reported.

Use the global --output=json flag to get the history as JSON instead of plain text.

Example:
  skima inspect --hist
  skima inspect --hist --output=json`,
	Run: func(cmd *cobra.Command, args []string) {
		if hist {
			retrieveHist()
		}
	},
}

func init() {
	rootCmd.AddCommand(inspectCmd)

	//hist is a package level variable which is defined in another file - but still accessible
	inspectCmd.Flags().BoolVar(&hist, "hist", false, "Reads the history tables for info on the application of a version of the schema")
	inspectCmd.Flags().StringVar(&histVer, "version", "", "(not yet implemented) When reading the history, target this specific version of the schema. Default behavior is the latest version")
	inspectCmd.Flags().BoolVar(&allHist, "allhist", false, "(not yet implemented) If you want to get the full history of attempts to apply a version of the schema, not just successful results")

}

func retrieveHist() {
	applyHist, err := db.GetApplyHistory()
	if err == db.NoHistTable {
		fmt.Println("Schema is not managed by skima - run 'skima apply' to deploy it, or 'skima apply --hist' to only create the history tables")
	} else if err == pgx.ErrNoRows {
		fmt.Println("No successful builds found")
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "Unable to retrieve history", err)
	} else {
		if len(output) > 0 {
			jsonOutput, err := json.Marshal(applyHist)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Unable to marshal history object into JSON", err)
			} else {
				fmt.Println(string(jsonOutput))
			}
		} else {
			fmt.Println(applyHist.String())
		}
	}
}
