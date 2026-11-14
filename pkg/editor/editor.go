// Package editor reads and edits the state files - the edits behind the state editor API (see
// docs/specs/state-editor-api.md). Every edit is applied to an in-memory copy of the state, validated, and only then
// written: the files it changed are rewritten in the canonical format, and the rest are left untouched.
package editor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"skima/pkg/manifest"
	"skima/pkg/util"
	"sort"
	"strings"
	"sync"
)

var (
	// ErrNotFound is returned when a table, column or other object does not exist
	ErrNotFound = errors.New("not found")
	// ErrExists is returned when creating something that already exists
	ErrExists = errors.New("already exists")
	// ErrStale is returned when the file changed since its ETag was read - e.g. it was edited by hand meanwhile
	ErrStale = errors.New("the file changed since it was read")
	// ErrInvalidName is returned for a name that can't be a postgres identifier or a state file name
	ErrInvalidName = errors.New("invalid name")
)

// new tables are named like postgres unquoted identifiers, since skima's DDL doesn't quote them
var newNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_$]{0,62}$`)

// existing names only need to be safe to use in a file name
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_$]+$`)

// Issue is a validation problem in one of the state files
type Issue struct {
	File string `json:"file"`
	manifest.ValidationIssue
}

// InvalidEditError is returned when an edit would make the state invalid. Only problems the edit introduces count -
// problems that were already there are returned as warnings instead
type InvalidEditError struct {
	Issues []Issue
}

func (e *InvalidEditError) Error() string {
	return fmt.Sprintf("the edit would make the state invalid (%d problems)", len(e.Issues))
}

// ReferencedError is returned when dropping something that other objects use, unless the drop cascades
type ReferencedError struct {
	What       string
	References []string
}

func (e *ReferencedError) Error() string {
	return fmt.Sprintf("%s is used by %s - drop those first, or cascade", e.What, strings.Join(e.References, ", "))
}

// Rename is a column renamed by an edit - recorded so the generated changeset renames the column instead of dropping
// and adding it
type Rename struct {
	Table string `json:"table"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Result is what an edit changed
type Result struct {
	// Table is the edited table after the edit - nil when it was dropped
	Table *manifest.Table
	// ETag is the edited table's new ETag - empty when it was dropped
	ETag string
	// Written and Deleted are the files the edit changed, relative to the working directory
	Written []string
	Deleted []string
	Renames []Rename
	// Warnings are things to check by hand (e.g. SQL that mentions a renamed column) and validation problems that
	// were there before the edit
	Warnings []string
}

// ETag is a version token for a file's content
func ETag(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:8])
}

// Editor reads and edits the state files of a working directory. Edits are serialized
type Editor struct {
	workdir string
	mu      sync.Mutex
}

// New returns an editor for the state files in workdir
func New(workdir string) *Editor {
	return &Editor{workdir: workdir}
}

var kindSuffix = map[manifest.FileKind]string{
	manifest.KindTable:    "-table.json",
	manifest.KindView:     "-view.json",
	manifest.KindEnum:     "-enum.json",
	manifest.KindSequence: "-sequence.json",
	manifest.KindMatview:  "-matview.json",
}

func (e *Editor) path(kind manifest.FileKind, name string) string {
	return filepath.Join(e.workdir, "state", name+kindSuffix[kind])
}

func (e *Editor) relative(path string) string {
	if rel, err := filepath.Rel(e.workdir, path); err == nil {
		return rel
	}
	return path
}

// Names returns the names of the state objects of a kind, sorted
func (e *Editor) Names(kind manifest.FileKind) ([]string, error) {
	suffix, ok := kindSuffix[kind]
	if !ok {
		return nil, fmt.Errorf("%w: kind %q", ErrNotFound, kind)
	}
	paths, err := filepath.Glob(filepath.Join(e.workdir, "state", "*"+suffix))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(path), suffix))
	}
	sort.Strings(names)
	return names, nil
}

// Doc is a state file's content and ETag
type Doc struct {
	Kind    manifest.FileKind
	Name    string
	Path    string
	Content []byte
	ETag    string
}

// Get returns a state object's file
func (e *Editor) Get(kind manifest.FileKind, name string) (Doc, error) {
	if _, ok := kindSuffix[kind]; !ok {
		return Doc{}, fmt.Errorf("%w: kind %q", ErrNotFound, kind)
	}
	if !namePattern.MatchString(name) {
		return Doc{}, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	path := e.path(kind, name)
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Doc{}, fmt.Errorf("%w: %s '%s'", ErrNotFound, kind, name)
	} else if err != nil {
		return Doc{}, err
	}
	return Doc{Kind: kind, Name: name, Path: e.relative(path), Content: content, ETag: ETag(content)}, nil
}

// Table returns a table and its ETag
func (e *Editor) Table(name string) (manifest.Table, string, error) {
	doc, err := e.Get(manifest.KindTable, name)
	if err != nil {
		return manifest.Table{}, "", err
	}
	var table manifest.Table
	if err = json.Unmarshal(doc.Content, &table); err != nil {
		return manifest.Table{}, "", fmt.Errorf("parsing %s: %w", doc.Path, err)
	}
	return table, doc.ETag, nil
}

// file is a state file loaded for an edit
type file struct {
	path   string
	raw    []byte
	schema string
}

// working is the in-memory copy of the state an edit changes: the tables, and the sequences columns can own
type working struct {
	tables     map[string]*manifest.Table
	sequences  map[string]*manifest.Sequence
	tableFiles map[string]file
	seqFiles   map[string]file
	otherSql   map[string]string // view and materialized view sql by file, to warn about mentions
	result     *Result
}

func (w *working) table(name string) (*manifest.Table, error) {
	t, ok := w.tables[name]
	if !ok {
		return nil, fmt.Errorf("%w: table '%s'", ErrNotFound, name)
	}
	return t, nil
}

func (w *working) warn(format string, args ...any) {
	w.result.Warnings = append(w.result.Warnings, fmt.Sprintf(format, args...))
}

func readSchema(raw []byte) string {
	var header struct {
		Schema string `json:"$schema"`
	}
	_ = json.Unmarshal(raw, &header)
	return header.Schema
}

func (e *Editor) load() (*working, error) {
	w := &working{tables: map[string]*manifest.Table{}, sequences: map[string]*manifest.Sequence{},
		tableFiles: map[string]file{}, seqFiles: map[string]file{}, otherSql: map[string]string{}, result: &Result{}}
	for _, kind := range []manifest.FileKind{manifest.KindTable, manifest.KindSequence, manifest.KindView, manifest.KindMatview} {
		names, err := e.Names(kind)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			path := e.path(kind, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			f := file{path: path, raw: raw, schema: readSchema(raw)}
			switch kind {
			case manifest.KindTable:
				t := &manifest.Table{}
				if err = json.Unmarshal(raw, t); err != nil {
					return nil, fmt.Errorf("parsing %s: %w", e.relative(path), err)
				}
				w.tables[name], w.tableFiles[name] = t, f
			case manifest.KindSequence:
				s := &manifest.Sequence{}
				if err = json.Unmarshal(raw, s); err != nil {
					return nil, fmt.Errorf("parsing %s: %w", e.relative(path), err)
				}
				w.sequences[name], w.seqFiles[name] = s, f
			default:
				var query struct{ Sql string }
				_ = json.Unmarshal(raw, &query)
				w.otherSql[e.relative(path)] = query.Sql
			}
		}
	}
	return w, nil
}

func cloneTable(t *manifest.Table) *manifest.Table {
	raw, _ := json.Marshal(t)
	clone := &manifest.Table{}
	_ = json.Unmarshal(raw, clone)
	return clone
}

func cloneSequence(s *manifest.Sequence) *manifest.Sequence {
	raw, _ := json.Marshal(s)
	clone := &manifest.Sequence{}
	_ = json.Unmarshal(raw, clone)
	return clone
}

// edit applies op to a copy of the state, validates the result and writes the files it changed. target is the table
// the edit is about; etag, when not empty, must match its file
func (e *Editor) edit(target string, etag string, op func(w *working) error) (Result, error) {
	if !namePattern.MatchString(target) {
		return Result{}, fmt.Errorf("%w: %q", ErrInvalidName, target)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	original, err := e.load()
	if err != nil {
		return Result{}, err
	}
	if f, ok := original.tableFiles[target]; ok && etag != "" && etag != ETag(f.raw) {
		return Result{}, fmt.Errorf("%w: %s", ErrStale, e.relative(f.path))
	}
	w := &working{tables: map[string]*manifest.Table{}, sequences: map[string]*manifest.Sequence{},
		tableFiles: original.tableFiles, seqFiles: original.seqFiles, otherSql: original.otherSql, result: &Result{}}
	for name, t := range original.tables {
		w.tables[name] = cloneTable(t)
	}
	for name, s := range original.sequences {
		w.sequences[name] = cloneSequence(s)
	}
	if err = op(w); err != nil {
		return Result{}, err
	}

	// what changed: files to write (formatted) and files to delete, by path
	writes := map[string][]byte{}
	var deletes []string
	for name, t := range w.tables {
		if orig, ok := original.tables[name]; ok && reflect.DeepEqual(orig, t) {
			continue
		}
		f := original.tableFiles[name]
		if f.path == "" {
			f.path = e.path(manifest.KindTable, name)
		}
		content, err := manifest.FormatTable(*t, f.schema)
		if err != nil {
			return Result{}, err
		}
		writes[f.path] = content
	}
	for name := range original.tables {
		if _, ok := w.tables[name]; !ok {
			deletes = append(deletes, original.tableFiles[name].path)
		}
	}
	for name, s := range w.sequences {
		if reflect.DeepEqual(original.sequences[name], s) {
			continue
		}
		content, err := manifest.FormatSequence(*s, original.seqFiles[name].schema)
		if err != nil {
			return Result{}, err
		}
		writes[original.seqFiles[name].path] = content
	}
	for name := range original.sequences {
		if _, ok := w.sequences[name]; !ok {
			deletes = append(deletes, original.seqFiles[name].path)
		}
	}

	if err = e.validate(writes, deletes, w.result); err != nil {
		return Result{}, err
	}
	paths := make([]string, 0, len(writes))
	for path := range writes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err = util.WriteFileAtomic(path, writes[path], 0o644); err != nil {
			return Result{}, err
		}
		w.result.Written = append(w.result.Written, e.relative(path))
	}
	sort.Strings(deletes)
	for _, path := range deletes {
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return Result{}, err
		}
		w.result.Deleted = append(w.result.Deleted, e.relative(path))
	}
	if t, ok := w.tables[target]; ok {
		w.result.Table = t
		if content, written := writes[e.path(manifest.KindTable, target)]; written {
			w.result.ETag = ETag(content)
		} else {
			w.result.ETag = ETag(original.tableFiles[target].raw)
		}
	}
	return *w.result, nil
}

// validate checks the state files as they would be after the edit, in a scratch copy of the state directory. The edit
// is rejected if it adds a validation error; errors that were there before become warnings
func (e *Editor) validate(writes map[string][]byte, deletes []string, result *Result) error {
	before, err := e.scratchValidate(nil, nil)
	if err != nil {
		return err
	}
	after, err := e.scratchValidate(writes, deletes)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for _, issue := range before {
		existing[issue.File+"|"+issue.Location+"|"+issue.Message] = true
	}
	var introduced []Issue
	for _, issue := range after {
		if existing[issue.File+"|"+issue.Location+"|"+issue.Message] {
			result.Warnings = append(result.Warnings, fmt.Sprintf("existing problem in %s %s: %s", issue.File, issue.Location, issue.Message))
		} else {
			introduced = append(introduced, issue)
		}
	}
	if len(introduced) > 0 {
		return &InvalidEditError{Issues: introduced}
	}
	return nil
}

// scratchValidate copies the state files to a temporary directory, applies writes and deletes there, and returns the
// validation errors in the state files
func (e *Editor) scratchValidate(writes map[string][]byte, deletes []string) ([]Issue, error) {
	scratch, err := os.MkdirTemp("", "skima-edit-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	if err = os.Mkdir(filepath.Join(scratch, "state"), 0o755); err != nil {
		return nil, err
	}
	stateJson, err := filepath.Glob(filepath.Join(e.workdir, "state", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range stateJson {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err = os.WriteFile(filepath.Join(scratch, "state", filepath.Base(path)), content, 0o644); err != nil {
			return nil, err
		}
	}
	for path, content := range writes {
		if err = os.WriteFile(filepath.Join(scratch, "state", filepath.Base(path)), content, 0o644); err != nil {
			return nil, err
		}
	}
	for _, path := range deletes {
		if err = os.Remove(filepath.Join(scratch, "state", filepath.Base(path))); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	results, err := manifest.ValidateManifest(scratch, nil)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	for _, r := range results {
		for _, issue := range r.Issues {
			if issue.Severity == manifest.SeverityError {
				issues = append(issues, Issue{File: filepath.Join("state", filepath.Base(r.File)), ValidationIssue: issue})
			}
		}
	}
	return issues, nil
}
