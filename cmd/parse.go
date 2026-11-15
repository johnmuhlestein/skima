package cmd

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/johnmuhlestein/skima/internal/manifest"
	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newParseCmd() *cobra.Command {
	var table string
	cmd := &cobra.Command{
		Use:   "parse",
		Short: "Parses manifest files and shows what they contain.",
		Long: `Parses manifest files without connecting to a database - useful for checking
a definition before applying it.

Use --table to parse a table's state definition and print the DDL that would be
generated for it, including its foreign keys.

Without --table, the highest-versioned changeset in the "deltas" directory is
parsed and each of its deltas is printed.

Example:
  skima manifest parse --table documents
  skima manifest parse`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if table == "" {
				return evalDeltas(out)
			}
			parsed, err := manifest.ParseTableByName(workdir(), table)
			if err != nil {
				return fmt.Errorf("unable to parse %s: %w", table, err)
			}
			fmt.Fprintln(out, "Table DDL:")
			fmt.Fprintln(out, parsed.GenerateDdl())
			fmt.Fprintln(out, "FKs:")
			for _, fk := range parsed.GenerateFkDdl() {
				fmt.Fprintln(out, fk)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&table, "table", "t", "", "A specific table you'd like to parse")
	return cmd
}

func evalDeltas(out io.Writer) error {
	fmt.Fprintln(out, "evaluating deltas...")
	deltas, err := manifest.ListFiles(filepath.Join(viper.GetString("workdir"), "deltas"), "json")
	if err != nil {
		return err
	}
	// sort by semantic version - directory order is by file name, which puts v1_0_9 after v1_0_10
	deltas = util.NewerVersions(util.ExtractVersion("0.0.0"), deltas)
	if len(deltas) == 0 {
		fmt.Fprintln(out, "No changeset files found")
		return nil
	}
	last := deltas[len(deltas)-1]
	fmt.Fprintf(out, "Parsing %s\n", filepath.Base(last))
	cs, err := manifest.ParseChangeSet(last)
	if err != nil {
		return fmt.Errorf("unable to parse %s: %w", filepath.Base(last), err)
	}
	for idx, delta := range cs.Deltas {
		fmt.Fprintf(out, "ChangeSet %d: \n", idx+1)
		fmt.Fprintf(out, "  Object type: %s\n", delta)
	}
	return nil
}
