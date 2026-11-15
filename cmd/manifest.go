package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newManifestCmd() *cobra.Command {
	return &cobra.Command{
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
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "working directory: %s\n", viper.GetString("workdir"))
		},
	}
}
