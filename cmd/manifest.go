package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var listArg bool

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

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// the --workdir flag is defined on the root command so every command can use it

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	//manifestCmd.Flags().BoolVarP(&listArg, "list", "l", false, "list manifest files")
}
