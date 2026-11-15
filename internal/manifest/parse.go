package manifest

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/spf13/viper"
)

// osReader reads files - a seam the tests replace with fixtures
type osReader interface {
	readFile(name string) ([]byte, error)
	readDir(name string) ([]os.DirEntry, error)
}

type osReadWrapper struct{}

// SkimaSchemaHistoryTable is the definition of skima's run history table
//
//go:embed skima_schema_history-table.json
var SkimaSchemaHistoryTable []byte

// SkimaSchemaStatementsTable is the definition of skima's statement history table
//
//go:embed skima_schema_statements-table.json
var SkimaSchemaStatementsTable []byte

func (orw osReadWrapper) readFile(name string) ([]byte, error) {
	b, e := os.ReadFile(name)
	return b, e
}

func (orw osReadWrapper) readDir(name string) ([]os.DirEntry, error) {
	b, e := os.ReadDir(name)
	return b, e
}

var reader osReader

func init() {
	reader = osReadWrapper{}
}

// ParseChangeSet takes a file path to a ChangeSet file and
// parses out the ChangeSet information including parsing the
// Deltas that are part of the ChangeSet
// An error is returned if there are any issues parsing the file
func ParseChangeSet(path string) (ChangeSet, error) {
	activeChangeSet := ChangeSet{}
	namePart := filepath.Base(path)
	csBytes, err := reader.readFile(path)
	var data []byte
	if err == nil {
		err = json.Unmarshal(csBytes, &activeChangeSet)
		activeChangeSet.Version = util.ExtractVersion(namePart).String()
		activeChangeSet.SemVer = util.ExtractVersion(activeChangeSet.Version)
		activeChangeSet.ParsedDeltas = make([]Delta, len(activeChangeSet.Deltas))
		activeChangeSet.Filename = filepath.Base(path)
		for idx, delta := range activeChangeSet.Deltas {
			// validation normally catches these, but it can be skipped (--skip-validation)
			fields, ok := delta.(map[string]interface{})
			if !ok {
				return activeChangeSet, fmt.Errorf("delta %d of %s is not a JSON object", idx+1, namePart)
			}
			objType, ok := fields["object"].(string)
			if !ok {
				return activeChangeSet, fmt.Errorf("delta %d of %s has no \"object\" - it must be a string such as table, column or script", idx+1, namePart)
			}
			var simple bool
			// script deltas are always SqlDeltas - one that points at a script file has none of the other markers
			if objType != "script" && fields["def"] == nil && fields["mods"] == nil && fields["sql"] == nil {
				simple = true
			}
			if simple {
				data, err = json.Marshal(delta)
				if err == nil {
					chg := SimpleDelta{}
					err = json.Unmarshal(data, &chg)
					if err == nil {
						activeChangeSet.ParsedDeltas[idx] = chg
					}
				}
				if err != nil {
					return activeChangeSet, fmt.Errorf("delta %d of %s: %w", idx+1, namePart, err)
				}
			} else {
				// this is the ugly stuff - marshal and unmarshal
				data, err = json.Marshal(delta)
				if err == nil {
					switch objType {
					case "fk":
						fkd := FkDelta{}
						err = json.Unmarshal(data, &fkd)
						if err == nil {
							activeChangeSet.ParsedDeltas[idx] = fkd
						}
					case "column":
						cd := ColumnDelta{}
						err = json.Unmarshal(data, &cd)
						if err == nil {
							activeChangeSet.ParsedDeltas[idx] = cd
						}
					default:
						sd := SqlDelta{}
						err = json.Unmarshal(data, &sd)
						if err == nil {
							activeChangeSet.ParsedDeltas[idx] = sd
						}
					}
				}
				if err != nil {
					return activeChangeSet, fmt.Errorf("delta %d of %s: %w", idx+1, namePart, err)
				}
			}
		}
	}
	return activeChangeSet, err
}

// Version returns a SemanticVersion for the manifest based on
// the delta files in the manifest
func Version() util.SemanticVersion {
	deltaFilePaths, _ := ListFiles(filepath.Join(viper.GetString("workdir"), "deltas"), "json")
	if len(deltaFilePaths) == 0 {
		return util.ExtractVersion("1.0.0")
	}
	// the manifest version is the highest changeset version, whatever its value
	ver := util.ExtractVersion(filepath.Base(deltaFilePaths[0]))
	for _, path := range deltaFilePaths[1:] {
		if fileVer := util.ExtractVersion(filepath.Base(path)); fileVer.Compare(ver) > 0 {
			ver = fileVer
		}
	}
	return ver
}

// ParseTableByName reads the table's state file from the configured working directory - or, for skima's history
// tables, their embedded definitions
func ParseTableByName(name string) (Table, error) {
	switch name {
	case "skima_schema_history":
		return parseTable(SkimaSchemaHistoryTable)
	case "skima_schema_statements":
		return parseTable(SkimaSchemaStatementsTable)
	default:
		path := filepath.Join(viper.GetString("workdir"), "state", fmt.Sprintf("%s-table.json", name))
		return ParseTableByPath(path)
	}
}

// ParseTableByPath reads a table state file
func ParseTableByPath(path string) (Table, error) {
	entityBytes, err := reader.readFile(path)
	if err == nil {
		return parseTable(entityBytes)
	}
	return Table{}, err
}

// ParseEnumByName reads the enum's state file from the configured working directory
func ParseEnumByName(name string) (Enum, error) {
	path := filepath.Join(viper.GetString("workdir"), "state", fmt.Sprintf("%s-enum.json", name))
	return ParseEnumByPath(path)
}

// ParseEnumByPath reads an enum state file
func ParseEnumByPath(path string) (Enum, error) {
	entityBytes, err := reader.readFile(path)
	if err != nil {
		return Enum{}, err
	}
	enum := Enum{}
	err = json.Unmarshal(entityBytes, &enum)
	return enum, err
}

// ParseSequenceByName reads the sequence's state file from the configured working directory
func ParseSequenceByName(name string) (Sequence, error) {
	path := filepath.Join(viper.GetString("workdir"), "state", fmt.Sprintf("%s-sequence.json", name))
	return ParseSequenceByPath(path)
}

// ParseSequenceByPath reads a sequence state file
func ParseSequenceByPath(path string) (Sequence, error) {
	entityBytes, err := reader.readFile(path)
	if err != nil {
		return Sequence{}, err
	}
	seq := Sequence{}
	err = json.Unmarshal(entityBytes, &seq)
	return seq, err
}

// ParseMatviewByName reads the materialized view's state file from the configured working directory
func ParseMatviewByName(name string) (Matview, error) {
	path := filepath.Join(viper.GetString("workdir"), "state", fmt.Sprintf("%s-matview.json", name))
	return ParseMatviewByPath(path)
}

// ParseMatviewByPath reads a materialized view state file
func ParseMatviewByPath(path string) (Matview, error) {
	entityBytes, err := reader.readFile(path)
	if err != nil {
		return Matview{}, err
	}
	mv := Matview{}
	err = json.Unmarshal(entityBytes, &mv)
	return mv, err
}

// ParseViewByName reads the view's state file from the configured working directory
func ParseViewByName(name string) (View, error) {
	path := filepath.Join(viper.GetString("workdir"), "state", fmt.Sprintf("%s-view.json", name))
	return ParseViewByPath(path)
}

// ParseViewByPath reads a view state file
func ParseViewByPath(path string) (View, error) {
	entityBytes, err := reader.readFile(path)
	if err == nil {
		return parseView(entityBytes)
	}
	return View{}, err
}

func parseTable(raw []byte) (Table, error) {
	table := Table{}
	err := json.Unmarshal(raw, &table)
	return table, err
}

func parseView(raw []byte) (View, error) {
	view := View{}
	err := json.Unmarshal(raw, &view)
	return view, err
}

// ListFiles returns a list of files from a given path with a provided suffix
func ListFiles(path string, sfx string) ([]string, error) {
	files := make([]string, 0)
	c, err := reader.readDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return files, nil // a missing directory (e.g. no deltas yet) has no files - other errors are returned
	}
	if err == nil {
		for _, entry := range c {
			if filepath.Ext(entry.Name()) == fmt.Sprintf(".%s", sfx) {
				files = append(files, filepath.Join(path, entry.Name()))
			}
		}
	}
	return files, err
}

// referenceIndexes returns the indexes of the reference table - or materialized view, when it has a state file instead
func (sd SimpleDelta) referenceIndexes() ([]Index, error) {
	t, err := sd.referenceTable()
	if err == nil {
		return t.Indexes, nil
	}
	if mv, mvErr := ParseMatviewByName(sd.Reference); mvErr == nil {
		return mv.Indexes, nil
	}
	return nil, err
}

func (sd SimpleDelta) referenceTable() (Table, error) {
	t, err := ParseTableByName(sd.Reference)
	if err != nil {
		return t, fmt.Errorf("unable to parse the reference table %s: %w", sd.Reference, err)
	}
	return t, nil
}

// FunctionFile returns the sql file a function add delta runs - the first of FunctionFilePaths that exists. When none
// can be read it returns the error reading the last of them
func (sd SimpleDelta) FunctionFile() (string, error) {
	var err error
	for _, path := range FunctionFilePaths(viper.GetString("workdir"), sd.Name) {
		if _, err = reader.readFile(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("unable to open the sql file for %s - expected %s: %w", sd.Name, strings.Join(FunctionFilePaths("", sd.Name), " or "), err)
}

// FunctionFilePaths returns where a function delta's sql file can be, in lookup order: state/sql/pre (where the state
// build also runs it) and then state/sql, where function files were originally read from
func FunctionFilePaths(workdir string, name string) []string {
	return []string{
		filepath.Join(workdir, "state", "sql", "pre", name+".sql"),
		filepath.Join(workdir, "state", "sql", name+".sql"),
	}
}

// ScriptPath returns where a script delta or hook's sql file is - in the deltas/scripts directory
func ScriptPath(workdir string, script string) string {
	return filepath.Join(workdir, "deltas", "scripts", script)
}

// readScript reads a script file, returning an error if it is missing or empty - applying it would fail
func readScript(script string) (string, error) {
	dat, err := reader.readFile(ScriptPath(viper.GetString("workdir"), script))
	if err != nil {
		return "", fmt.Errorf("unable to read the script %s: %w", script, err)
	}
	if len(dat) == 0 {
		return "", fmt.Errorf("the script %s is empty", script)
	}
	return string(dat), nil
}

// Statements returns every statement the delta runs - its sql, or the contents of its script file as one statement
func (sd SqlDelta) Statements() ([]string, error) {
	if len(sd.Script) > 0 {
		script, err := readScript(sd.Script)
		if err != nil {
			return nil, err
		}
		return []string{script}, nil
	}
	return sd.Sql, nil
}

// Statements returns the hook's sql, or the contents of its script file as one statement when it has no sql
func (hook Hook) Statements() ([]string, error) {
	if len(hook.Sql) > 0 {
		return hook.Sql, nil
	}
	if len(hook.Script) > 0 {
		script, err := readScript(hook.Script)
		if err != nil {
			return nil, err
		}
		return []string{script}, nil
	}
	return nil, nil
}
