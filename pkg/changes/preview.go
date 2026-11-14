package changes

import (
	"os"
	"path/filepath"
	"skima/pkg/plan"
)

// PreviewSql plans the deltas as a changeset and returns the statements apply would run - a script step shows as a
// comment naming the script, followed by its statement
func PreviewSql(deltas []Delta) ([]string, error) {
	statements := []string{}
	if len(deltas) == 0 {
		return statements, nil
	}
	content, err := Changeset{Deltas: deltas}.Format()
	if err != nil {
		return statements, err
	}
	dir, err := os.MkdirTemp("", "skima-preview-")
	if err != nil {
		return statements, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "next.json")
	if err = os.WriteFile(path, content, 0o644); err != nil {
		return statements, err
	}
	unit, _, err := plan.ForChangesetFile(path)
	if err != nil {
		return statements, err
	}
	for _, step := range unit.Steps {
		if step.Script != "" {
			statements = append(statements, "-- runs "+step.Script)
		}
		statements = append(statements, step.Sql...)
	}
	return statements, nil
}
