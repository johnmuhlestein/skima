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

var deltaManifest bool

// listCmd represents the list command
var listCmd = &cobra.Command{
	Use:   "list",
	Short: "Lists the state or delta files in the manifest.",
	Long: `Lists the files that make up the schema manifest in the working directory.

By default this lists the JSON state definitions in the "state" directory.
Use --delta to list the changeset files in the "deltas" directory instead.

Example:
  skima manifest list
  skima manifest list --delta`,
	Run: func(cmd *cobra.Command, args []string) {
		if deltaManifest {
			fmt.Println("Available Delta Files:")
			listManifestFiles("deltas")
		} else {
			fmt.Println("Available State Files:")
			listManifestFiles("state")
		}
	},
}

func init() {
	manifestCmd.AddCommand(listCmd)
	listCmd.Flags().BoolVar(&deltaManifest, "delta", false, "List the delta files")
}

func check(e error) {
	if e != nil {
		fmt.Println(e)
		panic(e)
	}
}

func listManifestFiles(ftype string) {
	path := filepath.Join(viper.GetString("workdir"), ftype)
	jsonFiles, err := manifest.ListFiles(path, "json")
	check(err)
	if ftype == "deltas" {
		// list changesets in the order they would be applied
		fmt.Println("CHANGESETS:")
		jsonFiles = util.NewerVersions(util.ExtractVersion("0.0.0"), jsonFiles)
	} else {
		fmt.Println("TABLE/VIEW DEFINITIONS:")
	}
	for _, j := range jsonFiles {
		fmt.Println("  ", filepath.Base(j))
	}
	if ftype != "state" {
		return
	}
	// state sql lives in pre (run before tables/views) and post (run after) subdirectories
	for _, phase := range []string{"pre", "post"} {
		path = filepath.Join(viper.GetString("workdir"), ftype, "sql", phase)
		if _, err = os.Stat(path); os.IsNotExist(err) {
			continue
		}
		sqlFiles, err := manifest.ListFiles(path, "sql")
		check(err)
		fmt.Printf("SQL (%s):\n", phase)
		for _, j := range sqlFiles {
			fmt.Println("  ", filepath.Base(j))
		}
	}
}
