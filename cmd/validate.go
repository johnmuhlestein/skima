package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"skima/pkg/manifest"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var fileKind string

// validateCmd represents the manifest validate command
var validateCmd = &cobra.Command{
	Use:   "validate [file...]",
	Short: "Validates manifest files against the skima JSON schemas.",
	Long: `Checks manifest files against the JSON schemas for table, view and changeset files without connecting to a
database. Unknown or misspelled properties, missing required properties, invalid values and invalid JSON are all
reported, with the location of each problem in the file.

With no arguments, every state and changeset file in the working directory is validated. JSON files in the state
directory that are not named <name>-table.json or <name>-view.json are reported too, since skima ignores them.

Files given as arguments are validated individually. Their kind comes from the file name (-table.json, -view.json,
or any .json file in a deltas directory) - use --type for files named otherwise.

The exit code is 1 if any file is invalid. Use the global --output=json flag for machine readable results.

skima apply runs the same validation before it makes any changes (see skima apply --help).

Example:
  skima manifest validate
  skima manifest validate deltas/v1_0_3.json state/users-table.json
  skima manifest validate --type changeset proposed-change.json
  skima manifest validate --output=json`,
	Run: func(cmd *cobra.Command, args []string) {
		var results []manifest.FileValidation
		if len(args) == 0 {
			var err error
			results, err = manifest.ValidateManifest(viper.GetString("workdir"))
			if err != nil {
				fmt.Fprintln(os.Stderr, "Unable to read the manifest:", err)
				os.Exit(1)
			}
		} else {
			for _, path := range args {
				kind := manifest.FileKind(fileKind)
				if kind == "" {
					var ok bool
					if kind, ok = manifest.KindOf(path); !ok {
						results = append(results, manifest.FileValidation{File: path, Issues: []manifest.ValidationIssue{{
							Message: "unable to tell the file kind from its name - use --type table, view or changeset",
						}}})
						continue
					}
				}
				results = append(results, manifest.ValidateFile(path, kind))
			}
		}
		if invalid := reportValidation(results, true, output == "json"); invalid > 0 {
			os.Exit(1)
		}
	},
}

func init() {
	manifestCmd.AddCommand(validateCmd)
	validateCmd.Flags().StringVar(&fileKind, "type", "", "The kind of every file given as an argument (table, view or changeset) - by default it comes from the file name")
}

// reportValidation prints the validation results as JSON, or as text listing every file when showValid is true and
// otherwise only the invalid ones. It returns the number of invalid files
func reportValidation(results []manifest.FileValidation, showValid bool, asJSON bool) int {
	invalid := 0
	for _, r := range results {
		if !r.Valid {
			invalid++
		}
	}
	if asJSON {
		jsonOutput, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to marshal the validation results into JSON", err)
		} else {
			fmt.Println(string(jsonOutput))
		}
		return invalid
	}
	for _, r := range results {
		file := displayPath(r.File)
		if r.Valid {
			if showValid {
				fmt.Printf("ok       %s\n", file)
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "INVALID  %s\n", file)
		for _, issue := range r.Issues {
			location := issue.Location
			if location == "" {
				location = "(file)"
			}
			fmt.Fprintf(os.Stderr, "         %s: %s\n", location, issue.Message)
		}
	}
	files := "files"
	if len(results) == 1 {
		files = "file"
	}
	fmt.Printf("%d %s checked: %d valid, %d invalid\n", len(results), files, len(results)-invalid, invalid)
	return invalid
}

// displayPath shows manifest files relative to the working directory
func displayPath(path string) string {
	if workdir := viper.GetString("workdir"); workdir != "" {
		if rel, err := filepath.Rel(workdir, path); err == nil && !filepath.IsAbs(rel) && rel[0] != '.' {
			return rel
		}
	}
	return path
}
