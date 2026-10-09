package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"skima/pkg/manifest"
	"skima/pkg/util"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var fileKind string
var sinceVersion string

// validateCmd represents the manifest validate command
var validateCmd = &cobra.Command{
	Use:   "validate [file...]",
	Short: "Validates manifest files against the skima JSON schemas and checks their references.",
	Long: `Checks manifest files without connecting to a database. Each file is validated against the JSON schema for
its kind (table, view or changeset): unknown or misspelled properties, missing required properties, invalid values
and invalid JSON are all reported, with the location of each problem in the file.

References between files are checked too:
  - state files: the table/view name matches the file name, and primary key, index, constraint and foreign key
    columns exist, as do the tables foreign keys reference
  - changesets: the file name is a version no other changeset shares, and each delta's definition can be found where
    applying it looks - e.g. a column add needs the column in the table's state file, a function add its sql file and
    a script delta its file in deltas/scripts. Otherwise the apply fails or the delta is silently skipped

Old changesets can legitimately reference definitions later removed from the state files, so reference problems in
changesets are warnings. Use --since with the deployed version to make them errors in the changesets still to apply.

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
  skima manifest validate --since 1.4.2
  skima manifest validate --output=json`,
	Run: func(cmd *cobra.Command, args []string) {
		var errorsAfter *util.SemanticVersion
		if sinceVersion != "" {
			since := util.ExtractVersion(sinceVersion)
			errorsAfter = &since
		}
		var results []manifest.FileValidation
		var err error
		if len(args) == 0 {
			results, err = manifest.ValidateManifest(viper.GetString("workdir"), errorsAfter)
		} else {
			kinds := map[string]manifest.FileKind{}
			var paths []string
			var unknown []manifest.FileValidation
			for _, path := range args {
				kind := manifest.FileKind(fileKind)
				if kind == "" {
					var ok bool
					if kind, ok = manifest.KindOf(path); !ok {
						unknown = append(unknown, manifest.FileValidation{File: path, Issues: []manifest.ValidationIssue{{
							Severity: manifest.SeverityError,
							Message:  "unable to tell the file kind from its name - use --type table, view or changeset",
						}}})
						continue
					}
				}
				kinds[path] = kind
				paths = append(paths, path)
			}
			results, err = manifest.ValidateSelected(viper.GetString("workdir"), paths, kinds, errorsAfter)
			results = append(results, unknown...)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to read the manifest:", err)
			os.Exit(1)
		}
		if invalid := reportValidation(results, true, output == "json"); invalid > 0 {
			os.Exit(1)
		}
	},
}

func init() {
	manifestCmd.AddCommand(validateCmd)
	validateCmd.Flags().StringVar(&sinceVersion, "since", "", "Report reference problems as errors in changesets newer than this version (e.g. the deployed version) - older ones are warnings")
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
	warnings := 0
	for _, r := range results {
		file := displayPath(r.File)
		switch {
		case !r.Valid:
			fmt.Fprintf(os.Stderr, "INVALID  %s\n", file)
		case r.Warnings() > 0:
			fmt.Fprintf(os.Stderr, "WARN     %s\n", file)
		default:
			if showValid {
				fmt.Printf("ok       %s\n", file)
			}
			continue
		}
		for _, issue := range r.Issues {
			location := issue.Location
			if location == "" {
				location = "(file)"
			}
			prefix := ""
			if issue.Severity == manifest.SeverityWarning {
				warnings++
				prefix = "warning: "
			}
			fmt.Fprintf(os.Stderr, "         %s%s: %s\n", prefix, location, issue.Message)
		}
	}
	files := "files"
	if len(results) == 1 {
		files = "file"
	}
	summary := fmt.Sprintf("%d %s checked: %d valid, %d invalid", len(results), files, len(results)-invalid, invalid)
	if warnings > 0 {
		plural := "s"
		if warnings == 1 {
			plural = ""
		}
		summary += fmt.Sprintf(", %d warning%s (changeset reference problems - use --since <deployed version> to make them errors in newer changesets)", warnings, plural)
	}
	fmt.Println(summary)
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
