package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"skima/pkg/manifest"
	"skima/pkg/util"

	"github.com/spf13/viper"

	"github.com/spf13/cobra"
)

var tablename string

// parseCmd represents the parse command
var parseCmd = &cobra.Command{
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
	Run: func(cmd *cobra.Command, args []string) {
		if len(tablename) > 0 {
			ptable, err := manifest.ParseTableByName(tablename)
			if err != nil {
				fmt.Printf("Unable to parse %s: %s\n", tablename, err)
				os.Exit(-1)
			}
			tableDdl := ptable.GenerateDdl()
			fkDdl := ptable.GenerateFkDdl()

			fmt.Println("Table DDL:")
			fmt.Println(tableDdl)
			fmt.Println("FKs:")
			for _, fk := range fkDdl {
				fmt.Println(fk)
			}
		} else {
			evalDeltas()
		}
	},
}

func init() {
	manifestCmd.AddCommand(parseCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// parseCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	parseCmd.Flags().StringVarP(&tablename, "table", "t", "", "A specific table you'd like to parse")
}

func evalDeltas() {
	fmt.Println("evaluating deltas...")
	deltas, err := manifest.ListFiles(filepath.Join(viper.GetString("workdir"), "deltas"), "json")
	check(err)
	// sort by semantic version - directory order is by file name, which puts v1_0_9 after v1_0_10
	deltas = util.NewerVersions(util.ExtractVersion("0.0.0"), deltas)
	if len(deltas) == 0 {
		fmt.Println("No changeset files found")
		return
	}
	last := deltas[len(deltas)-1]
	fmt.Printf("Parsing %s\n", filepath.Base(last))
	cs, err := manifest.ParseChangeSet(last)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to parse %s: %s\n", filepath.Base(last), err)
		os.Exit(1)
	}
	for idx, delta := range cs.Deltas {
		fmt.Printf("ChangeSet %d: \n", idx+1)
		fmt.Printf("  Object type: %s\n", delta)
	}
}
