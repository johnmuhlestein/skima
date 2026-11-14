// Package server is skima serve's local HTTP API: reading the state files, editing tables, and building changesets
// from the edits (see docs/specs/state-editor-api.md). It binds to localhost and has no authentication
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/johnmuhlestein/skima/internal/changes"
	"github.com/johnmuhlestein/skima/internal/editor"
)

// Server serves the API for one working directory
type Server struct {
	workdir     string
	baselineRef string
	corsOrigin  string
	editor      *editor.Editor
	// draftMu serializes changes to the draft's hints
	draftMu sync.Mutex
	mux     *http.ServeMux
	// patterns are the routes the API serves
	patterns []string
}

// Options configure the server
type Options struct {
	// BaselineRef is the default baseline git ref - empty for the last commit that changed deltas/
	BaselineRef string
	// CorsOrigin is an origin allowed to call the API from a browser (e.g. a UI dev server) - empty for none
	CorsOrigin string
}

// New returns the API handler for the state files in workdir
func New(workdir string, opts Options) *Server {
	s := &Server{workdir: workdir, baselineRef: opts.BaselineRef, corsOrigin: opts.CorsOrigin, editor: editor.New(workdir), mux: http.NewServeMux()}
	s.routes()
	return s
}

// maxBody limits request bodies - state objects are small
const maxBody = 1 << 20

// ServeHTTP checks the Host header, handles CORS and serves the API
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !localHost(r.Host) {
		// a page in a browser can make requests to a localhost server through a DNS name it controls (DNS rebinding):
		// only requests addressed to a localhost name are served
		writeError(w, http.StatusForbidden, "forbidden_host", fmt.Sprintf("requests must be addressed to localhost, 127.0.0.1 or [::1], not %q", r.Host), nil)
		return
	}
	if s.corsOrigin != "" && r.Header.Get("Origin") == s.corsOrigin {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", s.corsOrigin)
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
		h.Set("Access-Control-Allow-Headers", "Content-Type, If-Match")
		h.Set("Access-Control-Expose-Headers", "ETag")
		h.Add("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if !safeMethod(r.Method) && s.crossOrigin(r) {
		// the API has no authentication, so a page on another site must not be able to change anything through the
		// user's browser (CSRF) - browsers send Origin with every cross-origin request that isn't a GET or HEAD
		writeError(w, http.StatusForbidden, "forbidden_origin", fmt.Sprintf("requests from origin %q are not allowed - start skima serve with --cors-origin to allow a UI at another origin", r.Header.Get("Origin")), nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	s.mux.ServeHTTP(w, r)
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// crossOrigin reports whether a browser sent the request from a page at another origin than the API's own or the
// allowed --cors-origin. Requests without an Origin header don't come from a page - e.g. curl
func (s *Server) crossOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "cross-site"
	}
	if s.corsOrigin != "" && origin == s.corsOrigin {
		return false
	}
	u, err := url.Parse(origin)
	return err != nil || u.Host != r.Host
}

func localHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// apiError is the error format of every failed request
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Issues are validation problems, References are what uses an object that can't be dropped, Changes are changes no
	// delta is generated for
	Issues     any      `json:"issues,omitempty"`
	References []string `json:"references,omitempty"`
	Changes    any      `json:"changes,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, code string, message string, detail func(*apiError)) {
	e := apiError{Code: code, Message: message}
	if detail != nil {
		detail(&e)
	}
	writeJSON(w, status, map[string]apiError{"error": e})
}

// writeFailure maps an error from the editor or the changeset builder to its status code
func writeFailure(w http.ResponseWriter, err error) {
	var invalidEdit *editor.InvalidEditError
	var referenced *editor.ReferencedError
	var pending *changes.PendingChangesetsError
	var unsupported *changes.UnsupportedChangesError
	var invalidChangeset *changes.InvalidChangesetError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &invalidEdit):
		writeError(w, http.StatusUnprocessableEntity, "invalid_edit", "the edit would make the state invalid", func(e *apiError) { e.Issues = invalidEdit.Issues })
	case errors.As(err, &referenced):
		writeError(w, http.StatusConflict, "referenced", err.Error(), func(e *apiError) { e.References = referenced.References })
	case errors.Is(err, editor.ErrStale):
		writeError(w, http.StatusPreconditionFailed, "stale", err.Error()+" - read it again", nil)
	case errors.Is(err, editor.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, editor.ErrExists):
		writeError(w, http.StatusConflict, "exists", err.Error(), nil)
	case errors.Is(err, editor.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error(), nil)
	case errors.As(err, &pending):
		writeError(w, http.StatusConflict, "pending_changeset", err.Error(), nil)
	case errors.As(err, &unsupported):
		writeError(w, http.StatusConflict, "unsupported_changes", err.Error(), func(e *apiError) { e.Changes = unsupported.Changes })
	case errors.As(err, &invalidChangeset):
		writeError(w, http.StatusUnprocessableEntity, "invalid_changeset", "the generated changeset is invalid and was not written", func(e *apiError) { e.Issues = invalidChangeset.Issues })
	case errors.Is(err, changes.ErrNoChanges), errors.Is(err, changes.ErrNothingGenerated):
		writeError(w, http.StatusConflict, "no_changes", err.Error(), nil)
	case errors.Is(err, changes.ErrNoBaseline):
		writeError(w, http.StatusConflict, "no_baseline", err.Error(), nil)
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is too large", nil)
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
	}
}

// decode reads a JSON request body into v, rejecting unknown properties - a misspelled property is an error rather
// than silently ignored
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	// a page on another site can send text/plain or form bodies without the browser asking first (a CORS preflight),
	// but not application/json
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send the request body as Content-Type: application/json", nil)
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeFailure(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error(), nil)
		}
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: more than one JSON value", nil)
		return false
	}
	return true
}

// ifMatch returns the request's If-Match ETag. Edits require it, so they can't overwrite a change made since the
// object was read
func ifMatch(w http.ResponseWriter, r *http.Request) (string, bool) {
	tag := strings.TrimSpace(r.Header.Get("If-Match"))
	if tag == "" {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "send the ETag of the object you read in an If-Match header", nil)
		return "", false
	}
	return strings.Trim(strings.TrimPrefix(tag, "W/"), `"`), true
}

func setETag(w http.ResponseWriter, tag string) {
	if tag != "" {
		w.Header().Set("ETag", `"`+tag+`"`)
	}
}

// baseline resolves the request's baseline: ?baseline=<ref>, or the server's default
func (s *Server) baseline(r *http.Request) (changes.Baseline, error) {
	ref := r.URL.Query().Get("baseline")
	if ref == "" {
		ref = s.baselineRef
	}
	return changes.ResolveBaseline(s.workdir, ref)
}

// updateDraft changes the draft's hints, serialized with other changes to it
func (s *Server) updateDraft(change func(d *changes.Draft)) error {
	s.draftMu.Lock()
	defer s.draftMu.Unlock()
	draft, err := changes.LoadDraft(s.workdir)
	if err != nil {
		return err
	}
	change(&draft)
	return draft.Save(s.workdir)
}
