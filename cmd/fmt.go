package cmd

import (
	"errors"
	"fmt"

	"github.com/johnmuhlestein/skima/internal/manifest"
	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newFmtCmd() *cobra.Command {
	var fmtCheck bool
	cmd := &cobra.Command{
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
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			paths := args
			if len(paths) == 0 {
				var err error
				if paths, err = manifest.StateFiles(viper.GetString("workdir")); err != nil {
					return fmt.Errorf("unable to read the state files: %w", err)
				}
			}
			var formatted, unchanged, failed int
			for _, path := range paths {
				content, changed, err := manifest.FormatStateFile(path)
				var invalid *manifest.InvalidFileError
				switch {
				case errors.As(err, &invalid):
					failed++
					fmt.Fprintf(errOut, "INVALID  %s - fix it before formatting\n", displayPath(path))
					for _, issue := range invalid.Validation.Issues {
						location := issue.Location
						if location == "" {
							location = "(file)"
						}
						fmt.Fprintf(errOut, "         %s: %s\n", location, issue.Message)
					}
				case err != nil:
					failed++
					fmt.Fprintf(errOut, "ERROR    %s: %s\n", displayPath(path), err)
				case !changed:
					unchanged++
				case fmtCheck:
					formatted++
					fmt.Fprintf(out, "unformatted  %s\n", displayPath(path))
				default:
					if err := util.WriteFileAtomic(path, content, 0o644); err != nil {
						failed++
						fmt.Fprintf(errOut, "ERROR    %s: %s\n", displayPath(path), err)
						continue
					}
					formatted++
					fmt.Fprintf(out, "formatted    %s\n", displayPath(path))
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
			fmt.Fprintf(out, "%d %s: %d %s, %d already formatted, %d invalid\n", len(paths), files, formatted, verb, unchanged, failed)
			if failed > 0 || (fmtCheck && formatted > 0) {
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fmtCheck, "check", false, "List the files that are not formatted without changing them - the exit code is 1 if there are any")
	return cmd
}
