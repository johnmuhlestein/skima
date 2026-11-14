package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/johnmuhlestein/skima/internal/manifest"
	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var fmtCheck bool

// fmtCmd represents the manifest fmt command
var fmtCmd = &cobra.Command{
	Use:   "fmt [file...]",
	Short: "Rewrites state files in the canonical format.",
	Long: `Rewrites state files (tables, views, enums, sequences and materialized views) in one canonical format: 2-space
indent, properties in the order the README documents them, optional properties left out when unset, and nullable
always written. Formatting never changes what a file means - only how it is laid out. A file's "$schema" is kept.

skima serve writes state files in this format, so run skima manifest fmt once and commit the result before editing
through the API - otherwise the first edit to a hand-formatted file reformats all of it.

With no arguments, every state file in the working directory is formatted. Invalid files are reported and left
unchanged, since parsing them could drop content such as a misspelled property - fix them first (see skima manifest
validate). Changesets are not formatted.

With --check nothing is written: files that are not formatted are listed and the exit code is 1, for CI.

Example:
  skima manifest fmt
  skima manifest fmt state/users-table.json
  skima manifest fmt --check`,
	Run: func(cmd *cobra.Command, args []string) {
		paths := args
		if len(paths) == 0 {
			var err error
			if paths, err = manifest.StateFiles(viper.GetString("workdir")); err != nil {
				fmt.Fprintln(os.Stderr, "Unable to read the state files:", err)
				os.Exit(1)
			}
		}
		var formatted, unchanged, failed int
		for _, path := range paths {
			content, changed, err := manifest.FormatStateFile(path)
			var invalid *manifest.InvalidFileError
			switch {
			case errors.As(err, &invalid):
				failed++
				fmt.Fprintf(os.Stderr, "INVALID  %s - fix it before formatting\n", displayPath(path))
				for _, issue := range invalid.Validation.Issues {
					location := issue.Location
					if location == "" {
						location = "(file)"
					}
					fmt.Fprintf(os.Stderr, "         %s: %s\n", location, issue.Message)
				}
			case err != nil:
				failed++
				fmt.Fprintf(os.Stderr, "ERROR    %s: %s\n", displayPath(path), err)
			case !changed:
				unchanged++
			case fmtCheck:
				formatted++
				fmt.Printf("unformatted  %s\n", displayPath(path))
			default:
				if err := util.WriteFileAtomic(path, content, 0o644); err != nil {
					failed++
					fmt.Fprintf(os.Stderr, "ERROR    %s: %s\n", displayPath(path), err)
					continue
				}
				formatted++
				fmt.Printf("formatted    %s\n", displayPath(path))
			}
		}
		files := "files"
		if len(paths) == 1 {
			files = "file"
		}
		verb := "formatted"
		if fmtCheck {
			verb = "not formatted"
		}
		fmt.Printf("%d %s: %d %s, %d already formatted, %d invalid\n", len(paths), files, formatted, verb, unchanged, failed)
		if failed > 0 || (fmtCheck && formatted > 0) {
			os.Exit(1)
		}
	},
}

func init() {
	manifestCmd.AddCommand(fmtCmd)
	fmtCmd.Flags().BoolVar(&fmtCheck, "check", false, "List the files that are not formatted without changing them - the exit code is 1 if there are any")
}
