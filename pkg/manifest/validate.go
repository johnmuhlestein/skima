package manifest

import (
	"bytes"
	"cmp"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"skima/pkg/util"
	"slices"
	"sort"
	"strconv"
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
	KindEnum      FileKind = "enum"
	KindSequence  FileKind = "sequence"
	KindMatview   FileKind = "matview"
	KindChangeset FileKind = "changeset"
)

// issue severities - a file with any error is invalid, warnings are reported but do not fail validation
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// ValidationIssue is one problem found in a manifest file
type ValidationIssue struct {
	Severity string `json:"severity"`
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

// finish defaults issues without a severity to errors and sets Valid - true when there are no errors
func (f *FileValidation) finish() {
	f.Valid = true
	for i := range f.Issues {
		if f.Issues[i].Severity == "" {
			f.Issues[i].Severity = SeverityError
		}
		if f.Issues[i].Severity == SeverityError {
			f.Valid = false
		}
	}
}

// Warnings returns the number of warnings
func (f FileValidation) Warnings() int {
	count := 0
	for _, issue := range f.Issues {
		if issue.Severity == SeverityWarning {
			count++
		}
	}
	return count
}

var compileOnce sync.Once
var compiledSchemas map[FileKind]*jsonschema.Schema
var compileErr error

func schemaFor(kind FileKind) (*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		kinds := []FileKind{KindTable, KindView, KindEnum, KindSequence, KindMatview, KindChangeset}
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
	case strings.HasSuffix(base, "-enum.json"):
		return KindEnum, true
	case strings.HasSuffix(base, "-sequence.json"):
		return KindSequence, true
	case strings.HasSuffix(base, "-matview.json"):
		return KindMatview, true
	case filepath.Ext(base) == ".json" && filepath.Base(filepath.Dir(path)) == "deltas":
		return KindChangeset, true
	}
	return "", false
}

// ValidateFile validates a manifest file against the schema for its kind
func ValidateFile(path string, kind FileKind) FileValidation {
	result := validateSchema(path, kind)
	result.finish()
	return result
}

func validateSchema(path string, kind FileKind) FileValidation {
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
	case errors.As(err, &validationErr):
		result.Issues = leafIssues(validationErr, message.NewPrinter(language.English), nil)
		// the validator does not report the problems in a fixed order - list them in the order they appear in the file
		sort.SliceStable(result.Issues, func(i, j int) bool {
			if c := compareLocations(result.Issues[i].Location, result.Issues[j].Location); c != 0 {
				return c < 0
			}
			return result.Issues[i].Message < result.Issues[j].Message
		})
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
	"#/$defs/commentDelta/else/not":            "reference is only used for a column comment - it names the column's table",
	"#/$defs/enumDelta/else/not":               "value is only used when the action is alter - it names the value to add",
	"#/$defs/matviewDelta/else/not":            "concurrently is only used when the action is refresh",
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

// compareLocations orders JSON pointers by their tokens, comparing array indexes as numbers so /deltas/2 comes before
// /deltas/10
func compareLocations(a string, b string) int {
	at, bt := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(at) && i < len(bt); i++ {
		ai, aErr := strconv.Atoi(at[i])
		bi, bErr := strconv.Atoi(bt[i])
		if aErr == nil && bErr == nil {
			if ai != bi {
				return cmp.Compare(ai, bi)
			}
		} else if at[i] != bt[i] {
			return strings.Compare(at[i], bt[i])
		}
	}
	return cmp.Compare(len(at), len(bt))
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

// StateFiles returns the table, view, enum, sequence and materialized view state files in the working directory, the
// files a state build applies
func StateFiles(workdir string) ([]string, error) {
	var files []string
	for _, suffix := range []string{"-table.json", "-view.json", "-enum.json", "-sequence.json", "-matview.json"} {
		matches, err := filepath.Glob(filepath.Join(workdir, "state", "*"+suffix))
		if err != nil {
			return nil, err
		}
		files = append(files, matches...)
	}
	return files, nil
}

// ValidateManifest validates every state and changeset file in the working directory: against the JSON schemas, and
// that their references to other files resolve. JSON files in the state directory that are not named as a table or
// view are reported too, since skima would silently ignore them.
//
// Old changesets can legitimately reference definitions that were later removed from the state files, so reference
// problems in changesets are warnings - unless errorsAfter is set, which makes them errors in changesets newer than it
func ValidateManifest(workdir string, errorsAfter *util.SemanticVersion) ([]FileValidation, error) {
	stateFiles, err := StateFiles(workdir)
	if err != nil {
		return nil, err
	}
	deltaFiles, err := ListFiles(filepath.Join(workdir, "deltas"), "json")
	if err != nil {
		return nil, err
	}
	results := validateFiles(workdir, stateFiles, sortedByVersion(deltaFiles), deltaFiles, errorsAfter)

	allStateJson, err := filepath.Glob(filepath.Join(workdir, "state", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range allStateJson {
		if _, ok := KindOf(path); !ok {
			ignored := FileValidation{File: path, Issues: []ValidationIssue{{
				Message: "not a table (<name>-table.json), view (<name>-view.json), enum (<name>-enum.json), sequence (<name>-sequence.json) or materialized view (<name>-matview.json) file - skima ignores it",
			}}}
			ignored.finish()
			results = append(results, ignored)
		}
	}
	// state files by name, then changesets in the order they are applied
	sort.SliceStable(results, func(i, j int) bool {
		iState, jState := results[i].Kind != KindChangeset, results[j].Kind != KindChangeset
		if iState != jState {
			return iState
		}
		return iState && results[i].File < results[j].File
	})
	return results, nil
}

// ValidateForApply validates the state files and the changesets about to be applied - reference problems in them are
// errors, since they would make the apply fail or silently skip deltas
func ValidateForApply(workdir string, changesets []string) ([]FileValidation, error) {
	stateFiles, err := StateFiles(workdir)
	if err != nil {
		return nil, err
	}
	deltaFiles, err := ListFiles(filepath.Join(workdir, "deltas"), "json")
	if err != nil {
		return nil, err
	}
	allErrors := util.ExtractVersion("0.0.0")
	return validateFiles(workdir, stateFiles, changesets, deltaFiles, &allErrors), nil
}

// ValidateSelected validates the given files, checking their references against the working directory's state
// files. kinds gives each file's kind
func ValidateSelected(workdir string, paths []string, kinds map[string]FileKind, errorsAfter *util.SemanticVersion) ([]FileValidation, error) {
	stateFiles, err := StateFiles(workdir)
	if err != nil {
		return nil, err
	}
	deltaFiles, err := ListFiles(filepath.Join(workdir, "deltas"), "json")
	if err != nil {
		return nil, err
	}
	var changesets []string
	for _, path := range paths {
		if kinds[path] == KindChangeset {
			changesets = append(changesets, path)
		}
	}
	checked := map[string]FileValidation{}
	for _, result := range validateFiles(workdir, stateFiles, changesets, deltaFiles, errorsAfter) {
		if abs, err := filepath.Abs(result.File); err == nil {
			checked[abs] = result
		}
	}
	results := make([]FileValidation, 0, len(paths))
	for _, path := range paths {
		abs, _ := filepath.Abs(path)
		if result, ok := checked[abs]; ok && result.Kind == kinds[path] {
			result.File = path
			results = append(results, result)
		} else {
			// a state file outside the working directory, or validated as another kind - schema checks only
			results = append(results, ValidateFile(path, kinds[path]))
		}
	}
	return results, nil
}

func validateFiles(workdir string, stateFiles []string, changesets []string, allDeltaFiles []string, errorsAfter *util.SemanticVersion) []FileValidation {
	results := make([]FileValidation, 0, len(stateFiles)+len(changesets))
	for _, path := range stateFiles {
		kind, _ := KindOf(path)
		result := validateSchema(path, kind)
		result.finish()
		results = append(results, result)
	}
	statePtrs := make([]*FileValidation, len(results))
	for i := range results {
		statePtrs[i] = &results[i]
	}
	index := indexState(statePtrs)
	for _, result := range statePtrs {
		checkStateReferences(result, index)
		result.finish()
	}
	for _, path := range changesets {
		result := validateSchema(path, KindChangeset)
		result.finish()
		severity := SeverityWarning
		if errorsAfter != nil && util.ExtractVersion(filepath.Base(path)).Compare(*errorsAfter) > 0 {
			severity = SeverityError
		}
		checkChangesetName(&result, allDeltaFiles)
		checkChangesetReferences(&result, index, workdir, severity)
		result.finish()
		results = append(results, result)
	}
	return results
}

// sortedByVersion orders changeset files by version, keeping files with the same version (unlike util.NewerVersions)
func sortedByVersion(paths []string) []string {
	sorted := slices.Clone(paths)
	sort.SliceStable(sorted, func(i, j int) bool {
		vi, vj := util.ExtractVersion(filepath.Base(sorted[i])), util.ExtractVersion(filepath.Base(sorted[j]))
		if c := vi.Compare(vj); c != 0 {
			return c < 0
		}
		return sorted[i] < sorted[j]
	})
	return sorted
}
