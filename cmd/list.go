package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/johnmuhlestein/skima/internal/manifest"
	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newListCmd() *cobra.Command {
	var deltas bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Lists the state or delta files in the manifest.",
		Long: `Lists the files that make up the schema manifest in the working directory.

By default this lists the JSON state definitions in the "state" directory.
Use --delta to list the changeset files in the "deltas" directory instead.

Example:
  skima manifest list
  skima manifest list --delta`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if deltas {
				fmt.Fprintln(cmd.OutOrStdout(), "Available Delta Files:")
				return listManifestFiles(cmd.OutOrStdout(), "deltas")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Available State Files:")
			return listManifestFiles(cmd.OutOrStdout(), "state")
		},
	}
	cmd.Flags().BoolVar(&deltas, "delta", false, "List the delta files")
	return cmd
}

func listManifestFiles(out io.Writer, ftype string) error {
	path := filepath.Join(viper.GetString("workdir"), ftype)
	jsonFiles, err := manifest.ListFiles(path, "json")
	if err != nil {
		return err
	}
	if ftype == "deltas" {
		// list changesets in the order they would be applied
		fmt.Fprintln(out, "CHANGESETS:")
		jsonFiles = util.NewerVersions(util.ExtractVersion("0.0.0"), jsonFiles)
	} else {
		fmt.Fprintln(out, "TABLE/VIEW DEFINITIONS:")
	}
	for _, j := range jsonFiles {
		fmt.Fprintln(out, "  ", filepath.Base(j))
	}
	if ftype != "state" {
		return nil
	}
	// state sql lives in pre (run before tables/views) and post (run after) subdirectories
	for _, phase := range []string{"pre", "post"} {
		path = filepath.Join(viper.GetString("workdir"), ftype, "sql", phase)
		if _, err = os.Stat(path); os.IsNotExist(err) {
			continue
		}
		sqlFiles, err := manifest.ListFiles(path, "sql")
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "SQL (%s):\n", phase)
		for _, j := range sqlFiles {
			fmt.Fprintln(out, "  ", filepath.Base(j))
		}
	}
	return nil
}
