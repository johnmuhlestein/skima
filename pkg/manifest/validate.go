package manifest

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"skima/pkg/util"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

//go:embed schemas/*.schema.json
var schemaFiles embed.FS

// the schemas' $id base - they are also published in the repo so editors can use them via "$schema"
const schemaBaseURL = "https://raw.githubusercontent.com/johnmuhlestein/skima/main/pkg/manifest/schemas/"

// FileKind is the type of manifest file, which determines the schema it is validated against
type FileKind string

const (
	KindTable     FileKind = "table"
	KindView      FileKind = "view"
	KindChangeset FileKind = "changeset"
)

// ValidationIssue is one problem found in a manifest file
type ValidationIssue struct {
	// Location is a JSON pointer to the offending value within the file - empty for the whole document
	Location string `json:"location"`
	Message  string `json:"message"`
}

// FileValidation is the result of validating one manifest file
type FileValidation struct {
	File   string            `json:"file"`
	Kind   FileKind          `json:"kind,omitempty"`
	Valid  bool              `json:"valid"`
	Issues []ValidationIssue `json:"issues,omitempty"`
}

var compileOnce sync.Once
var compiledSchemas map[FileKind]*jsonschema.Schema
var compileErr error

func schemaFor(kind FileKind) (*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		kinds := []FileKind{KindTable, KindView, KindChangeset}
		for _, k := range kinds {
			name := fmt.Sprintf("%s.schema.json", k)
			raw, err := schemaFiles.ReadFile("schemas/" + name)
			if err == nil {
				var doc any
				if doc, err = jsonschema.UnmarshalJSON(bytes.NewReader(raw)); err == nil {
					err = compiler.AddResource(schemaBaseURL+name, doc)
				}
			}
			if err != nil {
				compileErr = fmt.Errorf("loading the %s schema: %w", k, err)
				return
			}
		}
		compiledSchemas = make(map[FileKind]*jsonschema.Schema)
		for _, k := range kinds {
			sch, err := compiler.Compile(fmt.Sprintf("%s%s.schema.json", schemaBaseURL, k))
			if err != nil {
				compileErr = fmt.Errorf("compiling the %s schema: %w", k, err)
				return
			}
			compiledSchemas[k] = sch
		}
	})
	if compileErr != nil {
		return nil, compileErr
	}
	sch, ok := compiledSchemas[kind]
	if !ok {
		return nil, fmt.Errorf("unknown manifest file kind %q", kind)
	}
	return sch, nil
}

// KindOf infers a manifest file's kind from its path: *-table.json and *-view.json are state files, and any other
// .json file in a deltas directory is a changeset
func KindOf(path string) (FileKind, bool) {
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(base, "-table.json"):
		return KindTable, true
	case strings.HasSuffix(base, "-view.json"):
		return KindView, true
	case filepath.Ext(base) == ".json" && filepath.Base(filepath.Dir(path)) == "deltas":
		return KindChangeset, true
	}
	return "", false
}

// ValidateFile validates a manifest file against the schema for its kind
func ValidateFile(path string, kind FileKind) FileValidation {
	result := FileValidation{File: path, Kind: kind}
	sch, err := schemaFor(kind)
	if err != nil {
		result.Issues = []ValidationIssue{{Message: err.Error()}}
		return result
	}
	raw, err := reader.readFile(path)
	if err != nil {
		result.Issues = []ValidationIssue{{Message: fmt.Sprintf("unable to read the file: %s", err)}}
		return result
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		result.Issues = []ValidationIssue{{Message: "invalid JSON: " + syntaxErrorMessage(raw, err)}}
		return result
	}
	err = sch.Validate(doc)
	var validationErr *jsonschema.ValidationError
	switch {
	case err == nil:
		result.Valid = true
	case errors.As(err, &validationErr):
		result.Issues = leafIssues(validationErr, message.NewPrinter(language.English), nil)
	default:
		result.Issues = []ValidationIssue{{Message: err.Error()}}
	}
	return result
}

// clearer messages for rules whose generic message does not say what to fix, keyed by the rule's schema location
var ruleMessages = map[string]string{
	"#/$defs/columnMods/oneOf":                 "set exactly one of datatype, nullable or default to true",
	"#/$defs/scriptDelta/oneOf":                "set exactly one of sql or script",
	"#/$defs/hook/oneOf":                       "set exactly one of sql or script",
	"#/$defs/column/dependentSchemas/generate": "generate requires primarykey: true",
	"#/$defs/index/anyOf":                      "set columns, or the full create index statement in ddl",
	"#/$defs/fkDelta/then/not":                 "an fk delta with def takes the table from tablename - remove name and reference",
	"#/$defs/fkDelta/else/not":                 "tablename is only used with def - use reference for the table",
	"#/$defs/columnDelta/allOf/0/else/not":     "newname is only used when the action is rename",
	"#/$defs/columnDelta/allOf/1/else/not":     "mods is only used when the action is alter",
}

// leafIssues flattens the validation error tree into its leaves - the inner nodes only say which combination of
// rules (allOf, if/then, ...) the leaves belong to. Rules with a clearer message are reported as one issue instead
func leafIssues(err *jsonschema.ValidationError, printer *message.Printer, issues []ValidationIssue) []ValidationIssue {
	if _, location, found := strings.Cut(err.SchemaURL, "#"); found {
		// the failing keyword (oneOf, not, ...) is reported separately from the schema location it sits in
		keys := []string{"#" + location}
		if keyword := err.ErrorKind.KeywordPath(); len(keyword) > 0 {
			keys = append(keys, "#"+location+"/"+strings.Join(keyword, "/"))
		} else if _, isNot := err.ErrorKind.(*kind.Not); isNot {
			keys = append(keys, "#"+location+"/not") // a failed "not" reports no keyword
		}
		for _, key := range keys {
			if msg, ok := ruleMessages[key]; ok {
				return append(issues, ValidationIssue{Location: jsonPointer(err.InstanceLocation), Message: msg})
			}
		}
	}
	if len(err.Causes) == 0 {
		return append(issues, ValidationIssue{Location: jsonPointer(err.InstanceLocation), Message: err.ErrorKind.LocalizedString(printer)})
	}
	for _, cause := range err.Causes {
		issues = leafIssues(cause, printer, issues)
	}
	return issues
}

// syntaxErrorMessage adds the line and column of a JSON syntax error
func syntaxErrorMessage(raw []byte, err error) string {
	var syntaxErr *json.SyntaxError
	if jsonErr := json.Unmarshal(raw, new(any)); errors.As(jsonErr, &syntaxErr) {
		// Offset counts the bytes read including the offending character
		before := raw[:max(syntaxErr.Offset-1, 0)]
		line := bytes.Count(before, []byte("\n")) + 1
		column := len(before) - bytes.LastIndexByte(before, '\n')
		return fmt.Sprintf("%s (line %d, column %d)", syntaxErr, line, column)
	}
	return err.Error()
}

func jsonPointer(tokens []string) string {
	var bld strings.Builder
	for _, token := range tokens {
		bld.WriteString("/")
		bld.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(token))
	}
	return bld.String()
}

// StateFiles returns the table and view state files in the working directory, the files a state build applies
func StateFiles(workdir string) ([]string, error) {
	tables, err := filepath.Glob(filepath.Join(workdir, "state", "*-table.json"))
	if err != nil {
		return nil, err
	}
	views, err := filepath.Glob(filepath.Join(workdir, "state", "*-view.json"))
	if err != nil {
		return nil, err
	}
	return append(tables, views...), nil
}

// ValidateManifest validates every state and changeset file in the working directory. JSON files in the state
// directory that are not named as a table or view are reported too, since skima would silently ignore them
func ValidateManifest(workdir string) ([]FileValidation, error) {
	var results []FileValidation
	stateFiles, err := StateFiles(workdir)
	if err != nil {
		return nil, err
	}
	for _, path := range stateFiles {
		kind, _ := KindOf(path)
		results = append(results, ValidateFile(path, kind))
	}
	allStateJson, err := filepath.Glob(filepath.Join(workdir, "state", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range allStateJson {
		if _, ok := KindOf(path); !ok {
			results = append(results, FileValidation{File: path, Issues: []ValidationIssue{{
				Message: "not a table (<name>-table.json) or view (<name>-view.json) file - skima ignores it",
			}}})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].File < results[j].File })

	deltaFiles, err := ListFiles(filepath.Join(workdir, "deltas"), "json")
	if err != nil {
		return nil, err
	}
	// changesets in the order they are applied
	for _, path := range util.NewerVersions(util.ExtractVersion("0.0.0"), deltaFiles) {
		results = append(results, ValidateFile(path, KindChangeset))
	}
	return results, nil
}
