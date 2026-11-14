package changes

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"skima/pkg/util"
	"slices"
)

// RenameHint records that a column was renamed - a diff alone sees a dropped and an added column
type RenameHint struct {
	Table string `json:"table"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// BackfillHint fills a new NOT NULL column on an existing table: from a SQL expression, or a script file in
// deltas/scripts
type BackfillHint struct {
	Table      string `json:"table"`
	Column     string `json:"column"`
	Expression string `json:"expression,omitempty"`
	Script     string `json:"script,omitempty"`
}

// Draft is the intent recorded while changing the state, which a diff can't infer. The changes themselves are the
// state files; the draft only holds these hints. It is local: skima git-ignores the .skima directory
type Draft struct {
	Renames   []RenameHint   `json:"renames,omitempty"`
	Backfills []BackfillHint `json:"backfills,omitempty"`
}

func draftDir(workdir string) string {
	return filepath.Join(workdir, ".skima")
}

// DraftPath is where the draft is kept
func DraftPath(workdir string) string {
	return filepath.Join(draftDir(workdir), "draft.json")
}

// LoadDraft reads the draft - empty when there is none
func LoadDraft(workdir string) (Draft, error) {
	var draft Draft
	content, err := os.ReadFile(DraftPath(workdir))
	if errors.Is(err, fs.ErrNotExist) {
		return draft, nil
	}
	if err != nil {
		return draft, err
	}
	err = json.Unmarshal(content, &draft)
	return draft, err
}

// Save writes the draft, or removes the file when the draft is empty. The .skima directory gets a .gitignore so it
// is never committed, wherever it is in the repository
func (d Draft) Save(workdir string) error {
	if len(d.Renames) == 0 && len(d.Backfills) == 0 {
		return ClearDraft(workdir)
	}
	if err := os.MkdirAll(draftDir(workdir), 0o755); err != nil {
		return err
	}
	ignore := filepath.Join(draftDir(workdir), ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, fs.ErrNotExist) {
		if err = os.WriteFile(ignore, []byte("*\n"), 0o644); err != nil {
			return err
		}
	}
	content, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return util.WriteFileAtomic(DraftPath(workdir), append(content, '\n'), 0o644)
}

// ClearDraft removes the draft
func ClearDraft(workdir string) error {
	if err := os.Remove(DraftPath(workdir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RecordRename records a column rename. Renames chain - a then b then c is one rename from a to c - and renaming a
// column back to its original name removes the hint. A backfill for the column follows it
func (d *Draft) RecordRename(table string, from string, to string) {
	for i := range d.Backfills {
		if d.Backfills[i].Table == table && d.Backfills[i].Column == from {
			d.Backfills[i].Column = to
		}
	}
	for i, r := range d.Renames {
		if r.Table == table && r.To == from {
			if r.From == to {
				d.Renames = slices.Delete(d.Renames, i, i+1)
			} else {
				d.Renames[i].To = to
			}
			return
		}
	}
	d.Renames = append(d.Renames, RenameHint{Table: table, From: from, To: to})
}

// RecordBackfill records how to fill a new NOT NULL column, replacing an earlier backfill for it
func (d *Draft) RecordBackfill(hint BackfillHint) {
	d.Backfills = slices.DeleteFunc(d.Backfills, func(b BackfillHint) bool { return b.Table == hint.Table && b.Column == hint.Column })
	d.Backfills = append(d.Backfills, hint)
}

// ForgetColumn removes the hints for a column that was dropped
func (d *Draft) ForgetColumn(table string, column string) {
	d.Renames = slices.DeleteFunc(d.Renames, func(r RenameHint) bool { return r.Table == table && r.To == column })
	d.Backfills = slices.DeleteFunc(d.Backfills, func(b BackfillHint) bool { return b.Table == table && b.Column == column })
}

// ForgetTable removes the hints for a table that was dropped
func (d *Draft) ForgetTable(table string) {
	d.Renames = slices.DeleteFunc(d.Renames, func(r RenameHint) bool { return r.Table == table })
	d.Backfills = slices.DeleteFunc(d.Backfills, func(b BackfillHint) bool { return b.Table == table })
}
