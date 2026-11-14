package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/johnmuhlestein/skima/internal/changes"
	"github.com/johnmuhlestein/skima/internal/editor"
	"github.com/johnmuhlestein/skima/internal/manifest"
	"github.com/johnmuhlestein/skima/internal/util"
	"github.com/johnmuhlestein/skima/internal/verify"
	"github.com/spf13/viper"
)

//go:embed openapi.json
var openAPI []byte

func (s *Server) routes() {
	m := routeRecorder{s}
	m.HandleFunc("GET /api/v1/openapi.json", s.getOpenAPI)
	m.HandleFunc("GET /api/v1/manifest", s.getManifest)
	m.HandleFunc("GET /api/v1/validation", s.getValidation)
	m.HandleFunc("GET /api/v1/state/{kind}", s.listState)
	m.HandleFunc("GET /api/v1/state/{kind}/{name}", s.getState)

	m.HandleFunc("POST /api/v1/state/tables", s.createTable)
	m.HandleFunc("PATCH /api/v1/state/tables/{table}", s.patchTable)
	m.HandleFunc("DELETE /api/v1/state/tables/{table}", s.dropTable)
	m.HandleFunc("POST /api/v1/state/tables/{table}/columns", s.addColumn)
	m.HandleFunc("PATCH /api/v1/state/tables/{table}/columns/{column}", s.patchColumn)
	m.HandleFunc("DELETE /api/v1/state/tables/{table}/columns/{column}", s.dropColumn)
	m.HandleFunc("PUT /api/v1/state/tables/{table}/primary-key", s.setPrimaryKey)
	m.HandleFunc("DELETE /api/v1/state/tables/{table}/primary-key", s.dropPrimaryKey)
	m.HandleFunc("PUT /api/v1/state/tables/{table}/{collection}/{name}", s.putTableObject)
	m.HandleFunc("DELETE /api/v1/state/tables/{table}/{collection}/{name}", s.dropTableObject)

	m.HandleFunc("GET /api/v1/changes", s.getChanges)
	m.HandleFunc("GET /api/v1/changes/hints", s.getHints)
	m.HandleFunc("DELETE /api/v1/changes/hints/{id}", s.deleteHint)
	m.HandleFunc("POST /api/v1/changes/verify", s.verifyChanges)
	m.HandleFunc("POST /api/v1/changesets", s.buildChangeset)
	m.HandleFunc("POST /api/v1/changesets/{name}/verify", s.verifyChangeset)

	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path+" - see /api/v1/openapi.json", nil)
	})
}

// routeRecorder registers the API's routes, recording their patterns - the OpenAPI document must describe each one
type routeRecorder struct {
	s *Server
}

func (rr routeRecorder) HandleFunc(pattern string, handler http.HandlerFunc) {
	rr.s.patterns = append(rr.s.patterns, pattern)
	rr.s.mux.HandleFunc(pattern, handler)
}

var kinds = map[string]manifest.FileKind{
	"tables":    manifest.KindTable,
	"views":     manifest.KindView,
	"enums":     manifest.KindEnum,
	"sequences": manifest.KindSequence,
	"matviews":  manifest.KindMatview,
}

func (s *Server) getOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openAPI)
}

// --- reading

func (s *Server) getManifest(w http.ResponseWriter, r *http.Request) {
	type validationSummary struct {
		Files    int `json:"files"`
		Valid    int `json:"valid"`
		Invalid  int `json:"invalid"`
		Warnings int `json:"warnings"`
	}
	info := struct {
		Workdir       string            `json:"workdir"`
		Baseline      *changes.Baseline `json:"baseline,omitempty"`
		BaselineError string            `json:"baselineError,omitempty"`
		LatestVersion string            `json:"latestVersion"`
		Counts        map[string]int    `json:"counts"`
		Validation    validationSummary `json:"validation"`
	}{Workdir: s.workdir, Counts: map[string]int{}}
	if baseline, err := s.baseline(r); err == nil {
		info.Baseline = &baseline
	} else {
		info.BaselineError = err.Error()
	}
	latest, err := changes.LatestVersion(s.workdir)
	if err != nil {
		writeFailure(w, err)
		return
	}
	info.LatestVersion = latest.String()
	for name, kind := range kinds {
		names, err := s.editor.Names(kind)
		if err != nil {
			writeFailure(w, err)
			return
		}
		info.Counts[name] = len(names)
	}
	results, err := manifest.ValidateManifest(s.workdir, nil)
	if err != nil {
		writeFailure(w, err)
		return
	}
	for _, result := range results {
		info.Validation.Files++
		if result.Valid {
			info.Validation.Valid++
		} else {
			info.Validation.Invalid++
		}
		info.Validation.Warnings += result.Warnings()
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) getValidation(w http.ResponseWriter, r *http.Request) {
	var errorsAfter *util.SemanticVersion
	if since := r.URL.Query().Get("since"); since != "" {
		v := util.ExtractVersion(since)
		errorsAfter = &v
	}
	results, err := manifest.ValidateManifest(s.workdir, errorsAfter)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": results})
}

func (s *Server) kind(w http.ResponseWriter, r *http.Request) (manifest.FileKind, bool) {
	kind, ok := kinds[r.PathValue("kind")]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown state kind "+r.PathValue("kind")+" - use tables, views, enums, sequences or matviews", nil)
	}
	return kind, ok
}

func (s *Server) listState(w http.ResponseWriter, r *http.Request) {
	kind, ok := s.kind(w, r)
	if !ok {
		return
	}
	names, err := s.editor.Names(kind)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"names": names})
}

// getState returns the state file as it is, with its ETag
func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	kind, ok := s.kind(w, r)
	if !ok {
		return
	}
	doc, err := s.editor.Get(kind, r.PathValue("name"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	setETag(w, doc.ETag)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(doc.Content)
}

// --- editing tables

type editResponse struct {
	// Table is the edited table in its file format - absent when it was dropped
	Table    json.RawMessage `json:"table,omitempty"`
	ETag     string          `json:"etag,omitempty"`
	Written  []string        `json:"written,omitempty"`
	Deleted  []string        `json:"deleted,omitempty"`
	Renames  []editor.Rename `json:"renames,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
}

func (s *Server) writeEdit(w http.ResponseWriter, status int, result editor.Result, err error) {
	if err != nil {
		writeFailure(w, err)
		return
	}
	response := editResponse{ETag: result.ETag, Written: result.Written, Deleted: result.Deleted, Renames: result.Renames, Warnings: result.Warnings}
	if result.Table != nil {
		table, err := manifest.FormatTable(*result.Table, "")
		if err != nil {
			writeFailure(w, err)
			return
		}
		response.Table = table
	}
	setETag(w, result.ETag)
	writeJSON(w, status, response)
}

// edited records the hints an edit implies, then writes its response. A failure to save a hint is reported as a
// warning - the edit itself is done
func (s *Server) edited(w http.ResponseWriter, status int, result editor.Result, err error, hints func(d *changes.Draft)) {
	if err == nil && hints != nil {
		if hintErr := s.updateDraft(hints); hintErr != nil {
			result.Warnings = append(result.Warnings, "the edit was made, but its hint could not be saved: "+hintErr.Error())
		}
	}
	s.writeEdit(w, status, result, err)
}

func (s *Server) createTable(w http.ResponseWriter, r *http.Request) {
	var table manifest.Table
	if !decode(w, r, &table) {
		return
	}
	result, err := s.editor.CreateTable(table)
	s.writeEdit(w, http.StatusCreated, result, err)
}

func (s *Server) patchTable(w http.ResponseWriter, r *http.Request) {
	etag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	var body struct {
		Comment *string `json:"comment"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Comment == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "nothing to change - send a comment (empty to remove it)", nil)
		return
	}
	result, err := s.editor.SetTableComment(r.PathValue("table"), etag, *body.Comment)
	s.writeEdit(w, http.StatusOK, result, err)
}

func (s *Server) dropTable(w http.ResponseWriter, r *http.Request) {
	etag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	table := r.PathValue("table")
	result, err := s.editor.DropTable(table, etag, cascade(r))
	s.edited(w, http.StatusOK, result, err, func(d *changes.Draft) { d.ForgetTable(table) })
}

func cascade(r *http.Request) bool {
	return r.URL.Query().Get("cascade") == "true"
}

type backfill struct {
	Expression string `json:"expression"`
	Script     string `json:"script"`
}

func (s *Server) addColumn(w http.ResponseWriter, r *http.Request) {
	etag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	var body struct {
		manifest.Column
		// Backfill fills a new NOT NULL column on a table that has rows - see the spec's intent hints
		Backfill *backfill `json:"backfill"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Backfill != nil && (body.Backfill.Expression == "") == (body.Backfill.Script == "") {
		writeError(w, http.StatusBadRequest, "bad_request", "a backfill needs exactly one of expression or script", nil)
		return
	}
	table := r.PathValue("table")
	result, err := s.editor.AddColumn(table, etag, body.Column)
	var hints func(d *changes.Draft)
	if body.Backfill != nil {
		hints = func(d *changes.Draft) {
			d.RecordBackfill(changes.BackfillHint{Table: table, Column: body.Name, Expression: body.Backfill.Expression, Script: body.Backfill.Script})
		}
	}
	s.edited(w, http.StatusCreated, result, err, hints)
}

func (s *Server) patchColumn(w http.ResponseWriter, r *http.Request) {
	etag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	var body struct {
		// Name renames the column - on its own, not with other changes
		Name *string `json:"name"`
		editor.ColumnChanges
	}
	if !decode(w, r, &body) {
		return
	}
	table, column := r.PathValue("table"), r.PathValue("column")
	if body.Name != nil && *body.Name != column {
		if body.ColumnChanges != (editor.ColumnChanges{}) {
			writeError(w, http.StatusBadRequest, "bad_request", "rename a column on its own - send the other changes in a second request", nil)
			return
		}
		result, err := s.editor.RenameColumn(table, etag, column, *body.Name)
		s.edited(w, http.StatusOK, result, err, func(d *changes.Draft) {
			for _, rename := range result.Renames {
				d.RecordRename(rename.Table, rename.From, rename.To)
			}
		})
		return
	}
	result, err := s.editor.AlterColumn(table, etag, column, body.ColumnChanges)
	s.writeEdit(w, http.StatusOK, result, err)
}

func (s *Server) dropColumn(w http.ResponseWriter, r *http.Request) {
	etag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	table, column := r.PathValue("table"), r.PathValue("column")
	result, err := s.editor.DropColumn(table, etag, column, cascade(r))
	s.edited(w, http.StatusOK, result, err, func(d *changes.Draft) { d.ForgetColumn(table, column) })
}

func (s *Server) setPrimaryKey(w http.ResponseWriter, r *http.Request) {
	etag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	var body struct {
		Columns []string `json:"columns"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Columns) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "send the primary key's columns - DELETE removes it", nil)
		return
	}
	result, err := s.editor.SetPrimaryKey(r.PathValue("table"), etag, body.Columns)
	s.writeEdit(w, http.StatusOK, result, err)
}

func (s *Server) dropPrimaryKey(w http.ResponseWriter, r *http.Request) {
	etag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	result, err := s.editor.DropPrimaryKey(r.PathValue("table"), etag, cascade(r))
	s.writeEdit(w, http.StatusOK, result, err)
}

// putTableObject adds or replaces a foreign key, index, constraint or trigger - the name in the body, when given,
// must match the path
func (s *Server) putTableObject(w http.ResponseWriter, r *http.Request) {
	etag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	table, name := r.PathValue("table"), r.PathValue("name")
	checkName := func(bodyName *string) bool {
		if *bodyName == "" {
			*bodyName = name
		}
		if *bodyName != name {
			writeError(w, http.StatusBadRequest, "bad_request", "the name in the body ("+*bodyName+") doesn't match the path ("+name+")", nil)
			return false
		}
		return true
	}
	var result editor.Result
	var err error
	switch r.PathValue("collection") {
	case "foreign-keys":
		var fk manifest.ForeignKey
		if !decode(w, r, &fk) || !checkName(&fk.Name) {
			return
		}
		result, err = s.editor.PutForeignKey(table, etag, fk)
	case "indexes":
		var idx manifest.Index
		if !decode(w, r, &idx) || !checkName(&idx.Name) {
			return
		}
		result, err = s.editor.PutIndex(table, etag, idx)
	case "constraints":
		var cons manifest.Constraint
		if !decode(w, r, &cons) || !checkName(&cons.Name) {
			return
		}
		result, err = s.editor.PutConstraint(table, etag, cons)
	case "triggers":
		var trg manifest.Trigger
		if !decode(w, r, &trg) || !checkName(&trg.Name) {
			return
		}
		result, err = s.editor.PutTrigger(table, etag, trg)
	default:
		unknownCollection(w, r)
		return
	}
	s.writeEdit(w, http.StatusOK, result, err)
}

func (s *Server) dropTableObject(w http.ResponseWriter, r *http.Request) {
	etag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	table, name := r.PathValue("table"), r.PathValue("name")
	var result editor.Result
	var err error
	switch r.PathValue("collection") {
	case "foreign-keys":
		result, err = s.editor.DropForeignKey(table, etag, name)
	case "indexes":
		result, err = s.editor.DropIndex(table, etag, name)
	case "constraints":
		result, err = s.editor.DropConstraint(table, etag, name)
	case "triggers":
		result, err = s.editor.DropTrigger(table, etag, name)
	default:
		unknownCollection(w, r)
		return
	}
	s.writeEdit(w, http.StatusOK, result, err)
}

func unknownCollection(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "unknown table object "+r.PathValue("collection")+" - use foreign-keys, indexes, constraints or triggers", nil)
}

// --- changes and changesets

// resolveBaseline writes the error response when the baseline can't be resolved
func (s *Server) resolveBaseline(w http.ResponseWriter, r *http.Request) (changes.Baseline, bool) {
	baseline, err := s.baseline(r)
	switch {
	case errors.Is(err, changes.ErrNoBaseline):
		writeFailure(w, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_baseline", err.Error(), nil)
	default:
		return baseline, true
	}
	return baseline, false
}

func (s *Server) getChanges(w http.ResponseWriter, r *http.Request) {
	baseline, ok := s.resolveBaseline(w, r)
	if !ok {
		return
	}
	draft, err := changes.LoadDraft(s.workdir)
	if err != nil {
		writeFailure(w, err)
		return
	}
	result, err := changes.Diff(s.workdir, baseline, draft)
	if err != nil {
		writeFailure(w, err)
		return
	}
	response := struct {
		changes.Result
		Sql      []string `json:"sql"`
		SqlError string   `json:"sqlError,omitempty"`
	}{Result: result}
	if response.Sql, err = changes.PreviewSql(s.workdir, result.Deltas); err != nil {
		response.SqlError = err.Error()
	}
	writeJSON(w, http.StatusOK, response)
}

// verifyChanges proves the generated changeset against the database - see verify.Run. It needs the database settings
// and the CREATE privilege; a changeset that doesn't verify is still a 200, with verified false
func (s *Server) verifyChanges(w http.ResponseWriter, r *http.Request) {
	if !verifyAvailable(w) {
		return
	}
	baseline, ok := s.resolveBaseline(w, r)
	if !ok {
		return
	}
	draft, err := changes.LoadDraft(s.workdir)
	if err != nil {
		writeFailure(w, err)
		return
	}
	report, err := verify.Run(s.workdir, baseline, draft)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "verify_unavailable", "unable to verify against the database: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// verifyAvailable checks a database is configured - connecting with none configured exits the process
func verifyAvailable(w http.ResponseWriter) bool {
	if viper.GetString("dbconn.connstring") == "" {
		writeError(w, http.StatusServiceUnavailable, "verify_unavailable", "no database is configured to verify against - set dbconn in the config file or SKM_DBCONN_* and restart skima serve", nil)
		return false
	}
	return true
}

// verifyChangeset proves a changeset file not committed yet against the database - typically the one changeset build
// wrote, edited by hand to change its intent. See verify.File
func (s *Server) verifyChangeset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, err := changes.PendingChangeset(s.workdir, name)
	var committed *changes.CommittedChangesetError
	switch {
	case strings.ContainsAny(name, `/\`) || errors.Is(err, changes.ErrChangesetNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such changeset: deltas/"+name, nil)
		return
	case errors.As(err, &committed):
		writeError(w, http.StatusConflict, "committed_changeset", err.Error(), nil)
		return
	case err != nil:
		writeFailure(w, err)
		return
	}
	// validated before the database is needed, so an invalid edit is reported without one
	err = changes.ValidateChangeset(s.workdir, path)
	var invalid *changes.InvalidChangesetError
	if errors.As(err, &invalid) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_changeset", name+" is invalid - fix it before verifying", func(e *apiError) { e.Issues = invalid.Issues })
		return
	} else if err != nil {
		writeFailure(w, err)
		return
	}
	if !verifyAvailable(w) {
		return
	}
	baseline, ok := s.resolveBaseline(w, r)
	if !ok {
		return
	}
	report, err := verify.File(s.workdir, baseline, path)
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusUnprocessableEntity, "invalid_changeset", name+" is invalid - fix it before verifying", func(e *apiError) { e.Issues = invalid.Issues })
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "verify_unavailable", "unable to verify against the database: "+err.Error(), nil)
	default:
		writeJSON(w, http.StatusOK, report)
	}
}

type hint struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Table      string `json:"table"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Column     string `json:"column,omitempty"`
	Expression string `json:"expression,omitempty"`
	Script     string `json:"script,omitempty"`
}

func renameID(r changes.RenameHint) string     { return "rename:" + r.Table + "." + r.From }
func backfillID(b changes.BackfillHint) string { return "backfill:" + b.Table + "." + b.Column }

func (s *Server) getHints(w http.ResponseWriter, r *http.Request) {
	draft, err := changes.LoadDraft(s.workdir)
	if err != nil {
		writeFailure(w, err)
		return
	}
	hints := []hint{}
	for _, r := range draft.Renames {
		hints = append(hints, hint{ID: renameID(r), Kind: "rename", Table: r.Table, From: r.From, To: r.To})
	}
	for _, b := range draft.Backfills {
		hints = append(hints, hint{ID: backfillID(b), Kind: "backfill", Table: b.Table, Column: b.Column, Expression: b.Expression, Script: b.Script})
	}
	writeJSON(w, http.StatusOK, map[string][]hint{"hints": hints})
}

// deleteHint discards a hint - e.g. to treat a rename as a drop and an add after all
func (s *Server) deleteHint(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	found := false
	err := s.updateDraft(func(d *changes.Draft) {
		for i, rename := range d.Renames {
			if renameID(rename) == id {
				d.Renames = append(d.Renames[:i], d.Renames[i+1:]...)
				found = true
				return
			}
		}
		for i, b := range d.Backfills {
			if backfillID(b) == id {
				d.Backfills = append(d.Backfills[:i], d.Backfills[i+1:]...)
				found = true
				return
			}
		}
	})
	switch {
	case err != nil:
		writeFailure(w, err)
	case !found:
		writeError(w, http.StatusNotFound, "not_found", "no hint "+id, nil)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) buildChangeset(w http.ResponseWriter, r *http.Request) {
	baseline, ok := s.resolveBaseline(w, r)
	if !ok {
		return
	}
	var body struct {
		Description            string `json:"description"`
		Bump                   string `json:"bump"`
		Version                string `json:"version"`
		Transaction            *bool  `json:"transaction"`
		AcknowledgeUnsupported bool   `json:"acknowledgeUnsupported"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Description) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "a changeset needs a description", nil)
		return
	}
	opts := changes.BuildOptions{Description: body.Description, Bump: body.Bump, Version: body.Version,
		NoTransaction: body.Transaction != nil && !*body.Transaction, AcknowledgeUnsupported: body.AcknowledgeUnsupported}
	s.draftMu.Lock() // the build reads and clears the draft
	result, err := changes.Build(s.workdir, baseline, opts)
	s.draftMu.Unlock()
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"file":        result.File,
		"version":     result.Version,
		"changeset":   json.RawMessage(result.Content),
		"warnings":    result.Diff.Warnings,
		"unsupported": result.Diff.Unsupported,
	})
}
