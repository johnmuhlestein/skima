package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"skima/pkg/util"
	"slices"
	"strings"
)

// changeset file names are versions: v<major>[_<minor>[_<patch>]][-<label>[_<n>]].json
var changesetNamePattern = regexp.MustCompile(`^v?\d+(_\d+){0,2}(-[A-Za-z][A-Za-z0-9]*(_\d+)?)?\.json$`)

// stateIndex holds the parsed state files, keyed by the name in their file name - the name deltas use to load them
type stateIndex struct {
	tables     map[string]Table
	tableFiles map[string]string
	views      map[string]bool
}

func stateName(path string, suffix string) string {
	return strings.TrimSuffix(filepath.Base(path), suffix)
}

// indexState parses the state files that passed schema validation
func indexState(stateResults []*FileValidation) stateIndex {
	index := stateIndex{tables: map[string]Table{}, tableFiles: map[string]string{}, views: map[string]bool{}}
	for _, result := range stateResults {
		if !result.Valid {
			continue
		}
		switch result.Kind {
		case KindTable:
			if table, err := ParseTableByPath(result.File); err == nil {
				name := stateName(result.File, "-table.json")
				index.tables[name] = table
				index.tableFiles[name] = result.File
			}
		case KindView:
			index.views[stateName(result.File, "-view.json")] = true
		}
	}
	return index
}

func (f *FileValidation) addIssue(severity string, location string, format string, args ...any) {
	f.Issues = append(f.Issues, ValidationIssue{Severity: severity, Location: location, Message: fmt.Sprintf(format, args...)})
}

func hasColumn(table Table, name string) bool {
	return slices.ContainsFunc(table.Columns, func(c Column) bool { return c.Name == name })
}

// checkColumns reports each name that is not a column of the table
func checkColumns(result *FileValidation, table Table, names []string, location string, what string) {
	for i, name := range names {
		if !hasColumn(table, name) {
			result.addIssue(SeverityError, fmt.Sprintf("%s/%d", location, i), "%s column '%s' is not a column of table '%s'", what, name, table.Name)
		}
	}
}

// checkStateReferences checks that a state file is consistent with its file name and the other state files
func checkStateReferences(result *FileValidation, index stateIndex) {
	if !result.Valid {
		return
	}
	switch result.Kind {
	case KindView:
		if view, err := ParseViewByPath(result.File); err == nil && view.Name != stateName(result.File, "-view.json") {
			result.addIssue(SeverityError, "/name", "name '%s' does not match the file name - deltas load this file as view '%s'", view.Name, stateName(result.File, "-view.json"))
		}
	case KindTable:
		fileName := stateName(result.File, "-table.json")
		table, ok := index.tables[fileName]
		if !ok {
			return
		}
		if table.Name != fileName {
			result.addIssue(SeverityError, "/name", "name '%s' does not match the file name - deltas load this file as table '%s'", table.Name, fileName)
		}
		seen := map[string]bool{}
		for i, column := range table.Columns {
			if seen[column.Name] {
				result.addIssue(SeverityError, fmt.Sprintf("/columns/%d/name", i), "column '%s' is defined more than once", column.Name)
			}
			seen[column.Name] = true
		}
		checkColumns(result, table, table.PrimaryKey, "/primary-key", "primary key")
		for i, fk := range table.ForeignKeys {
			location := fmt.Sprintf("/foreign-keys/%d", i)
			checkColumns(result, table, fk.Columns, location+"/columns", "foreign key")
			if strings.Contains(fk.Reference.Table, ".") {
				continue // a schema-qualified table is outside this manifest
			}
			target, ok := index.tables[fk.Reference.Table]
			if !ok {
				result.addIssue(SeverityError, location+"/reference/table", "references table '%s', which has no valid state file (state/%s-table.json)", fk.Reference.Table, fk.Reference.Table)
				continue
			}
			checkColumns(result, target, fk.Reference.Columns, location+"/reference/columns", "referenced")
		}
		for i, idx := range table.Indexes {
			checkColumns(result, table, idx.Columns, fmt.Sprintf("/indexes/%d/columns", i), "index")
		}
		for i, cons := range table.Constraints {
			checkColumns(result, table, cons.Columns, fmt.Sprintf("/constraints/%d/columns", i), "constraint")
		}
	}
}

// checkChangesetName checks that a changeset's file name is a version, and that no other changeset has the same
// version - only one of them would ever be applied
func checkChangesetName(result *FileValidation, allDeltaFiles []string) {
	if filepath.Base(filepath.Dir(result.File)) != "deltas" {
		return // e.g. a proposed changeset validated with --type before it is added to the manifest
	}
	base := filepath.Base(result.File)
	if !changesetNamePattern.MatchString(base) {
		result.addIssue(SeverityError, "", "file name is not a version - name changesets like v1_0_2.json or v1_0_2-rc_1.json")
		return
	}
	version := util.ExtractVersion(base)
	for _, other := range allDeltaFiles {
		if filepath.Base(other) != base && changesetNamePattern.MatchString(filepath.Base(other)) && util.ExtractVersion(filepath.Base(other)).Compare(version) == 0 {
			result.addIssue(SeverityError, "", "has the same version (%s) as %s - only one of them would be applied", version.String(), filepath.Base(other))
		}
	}
}

// checkChangesetReferences checks that each delta's definitions can be found where applying it looks for them -
// otherwise applying it fails, or generates no DDL and is silently skipped
func checkChangesetReferences(result *FileValidation, index stateIndex, workdir string, severity string) {
	if !result.Valid {
		return
	}
	changeset, err := ParseChangeSet(result.File)
	if err != nil {
		result.addIssue(SeverityError, "", "unable to parse the changeset: %s", err)
		return
	}
	for i, delta := range changeset.ParsedDeltas {
		location := fmt.Sprintf("/deltas/%d", i)
		if hooks, ok := delta.(interface{ Hooks() PrePostHook }); ok {
			if hook := hooks.Hooks().Pre; hook.Script != "" && len(hook.Sql) == 0 {
				checkScriptFile(result, workdir, hook.Script, location+"/pre/script", severity)
			}
			if hook := hooks.Hooks().Post; hook.Script != "" && len(hook.Sql) == 0 {
				checkScriptFile(result, workdir, hook.Script, location+"/post/script", severity)
			}
		}
		switch d := delta.(type) {
		case SqlDelta:
			if d.Script != "" {
				checkScriptFile(result, workdir, d.Script, location+"/script", severity)
			}
		case ColumnDelta:
			// an alter reads the column's definition from state; add/drop with def do not
			if d.Action == "alter" {
				if table, ok := requireTable(result, index, d.Reference, location, severity); ok && !hasColumn(table, d.Name) {
					result.addIssue(severity, location+"/name", "table '%s' has no column '%s' in %s", d.Reference, d.Name, displayFile(index, d.Reference))
				}
			}
		case SimpleDelta:
			if d.Action != "add" {
				continue // drop and rename do not read state
			}
			checkSimpleAdd(result, index, workdir, d, location, severity)
		}
	}
}

func checkSimpleAdd(result *FileValidation, index stateIndex, workdir string, d SimpleDelta, location string, severity string) {
	switch d.Object {
	case "table":
		if _, ok := index.tables[d.Name]; !ok {
			result.addIssue(severity, location+"/name", "table '%s' has no valid state file (state/%s-table.json)", d.Name, d.Name)
		}
	case "view":
		if !index.views[d.Name] {
			result.addIssue(severity, location+"/name", "view '%s' has no valid state file (state/%s-view.json)", d.Name, d.Name)
		}
	case "function":
		for _, path := range FunctionFilePaths(workdir, d.Name) {
			if _, err := os.Stat(path); err == nil {
				return
			}
		}
		result.addIssue(severity, location+"/name", "function '%s' has no sql file - expected %s", d.Name, strings.Join(FunctionFilePaths("", d.Name), " or "))
	case "column", "index", "trigger", "fk", "constraint", "pk":
		table, ok := requireTable(result, index, d.Reference, location, severity)
		if !ok {
			return
		}
		var found bool
		switch d.Object {
		case "column":
			found = hasColumn(table, d.Name)
		case "index":
			found = slices.ContainsFunc(table.Indexes, func(i Index) bool { return i.Name == d.Name })
		case "trigger":
			found = slices.ContainsFunc(table.Triggers, func(t Trigger) bool { return t.Name == d.Name })
		case "fk":
			found = slices.ContainsFunc(table.ForeignKeys, func(f ForeignKey) bool { return f.Name == d.Name })
		case "constraint":
			found = slices.ContainsFunc(table.Constraints, func(c Constraint) bool { return c.Name == d.Name })
		case "pk":
			if len(table.PrimaryKey) == 0 {
				result.addIssue(severity, location+"/reference", "table '%s' has no primary-key in %s", d.Reference, displayFile(index, d.Reference))
			}
			return
		}
		if !found {
			what := map[string]string{"column": "column", "index": "index", "trigger": "trigger", "fk": "foreign key", "constraint": "constraint"}[d.Object]
			result.addIssue(severity, location+"/name", "table '%s' has no %s '%s' in %s", d.Reference, what, d.Name, displayFile(index, d.Reference))
		}
	}
}

func requireTable(result *FileValidation, index stateIndex, name string, location string, severity string) (Table, bool) {
	table, ok := index.tables[name]
	if !ok {
		result.addIssue(severity, location+"/reference", "table '%s' has no valid state file (state/%s-table.json)", name, name)
	}
	return table, ok
}

func displayFile(index stateIndex, table string) string {
	return filepath.Join("state", filepath.Base(index.tableFiles[table]))
}

func checkScriptFile(result *FileValidation, workdir string, script string, location string, severity string) {
	path := filepath.Join(workdir, "deltas", "scripts", script)
	if _, err := os.Stat(path); err != nil {
		result.addIssue(severity, location, "script file %s does not exist", filepath.Join("deltas", "scripts", script))
	}
}
