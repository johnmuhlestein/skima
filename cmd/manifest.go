package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// manifestCmd represents the manifest command
var manifestCmd = &cobra.Command{
	Use:   "manifest",
	Short: "Works with the local schema manifest (state and delta files).",
	Long: `Commands for reading the schema manifest in the working directory without
connecting to a database.

The working directory contains a "state" directory (table/view definitions and SQL
files) and a "deltas" directory (versioned changesets). It is set by the "workdir"
config value, the SKM_WORKDIR environment variable or the --workdir flag.

Run on its own, this command prints the working directory in use.

Example:
  skima manifest --workdir ./myschema
  skima manifest list --delta
  skima manifest parse --table documents`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("working directory: %s\n", viper.GetString("workdir"))
	},
}

func init() {
	rootCmd.AddCommand(manifestCmd)

}
