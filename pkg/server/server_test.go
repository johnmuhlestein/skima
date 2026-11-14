package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// fixture commits a copy of the plan fixture manifest to a new git repository and returns a server for it
func fixture(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	workdir := filepath.Join(root, "schema")
	if err := os.CopyFS(workdir, os.DirFS("../plan/testdata/manifest")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "schema at 1.0.3"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	viper.Set("workdir", workdir) // the SQL preview plans with the configured working directory
	t.Cleanup(func() { viper.Set("workdir", "") })
	return New(workdir, opts), workdir
}

type response struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func call(t *testing.T, s *Server, method string, path string, body string, headers ...string) response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:7012"+path, reader)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	res := response{status: rec.Code, header: rec.Header(), raw: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &res.body)
	return res
}

// etag reads a table and returns its ETag for an If-Match header
func etag(t *testing.T, s *Server, table string) string {
	t.Helper()
	res := call(t, s, "GET", "/api/v1/state/tables/"+table, "")
	if res.status != http.StatusOK || res.header.Get("ETag") == "" {
		t.Fatalf("reading %s: %d %s", table, res.status, res.raw)
	}
	return res.header.Get("ETag")
}

func errorCode(res response) string {
	if e, ok := res.body["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code
	}
	return ""
}

func expect(t *testing.T, res response, status int, code string) {
	t.Helper()
	if res.status != status || errorCode(res) != code {
		t.Errorf("EXPECTED %d %s  ACTUAL: %d %s", status, code, res.status, res.raw)
	}
}

func TestHostCheck(t *testing.T) {
	s, _ := fixture(t, Options{})
	for host, status := range map[string]int{"localhost:7012": 200, "127.0.0.1:7012": 200, "[::1]:7012": 200, "localhost": 200,
		"evil.example.com": 403, "evil.example.com:7012": 403, "127.0.0.1.nip.io:7012": 403} {
		req := httptest.NewRequest("GET", "http://placeholder/api/v1/state/tables", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Errorf("Host %s - EXPECTED %d  ACTUAL: %d", host, status, rec.Code)
		}
	}
}

func TestRead(t *testing.T) {
	s, workdir := fixture(t, Options{})
	res := call(t, s, "GET", "/api/v1/state/tables", "")
	if names := res.body["names"]; res.status != 200 || len(names.([]any)) != 3 {
		t.Errorf("table names: %d %s", res.status, res.raw)
	}
	res = call(t, s, "GET", "/api/v1/state/sequences/tag_seq", "")
	content, _ := os.ReadFile(filepath.Join(workdir, "state", "tag_seq-sequence.json"))
	if res.status != 200 || res.raw != string(content) || !strings.HasPrefix(res.header.Get("ETag"), `"`) {
		t.Errorf("EXPECTED the file as it is, with a quoted ETag  ACTUAL: %d %s %s", res.status, res.header.Get("ETag"), res.raw)
	}
	expect(t, call(t, s, "GET", "/api/v1/state/widgets", ""), 404, "not_found")
	expect(t, call(t, s, "GET", "/api/v1/state/views/nope", ""), 404, "not_found")
	expect(t, call(t, s, "GET", "/api/v1/nope", ""), 404, "not_found")

	res = call(t, s, "GET", "/api/v1/manifest", "")
	counts, _ := res.body["counts"].(map[string]any)
	if res.status != 200 || res.body["latestVersion"] != "1.0.3" || counts["tables"] != 3.0 || res.body["baseline"] == nil {
		t.Errorf("manifest: %d %s", res.status, res.raw)
	}
	if res = call(t, s, "GET", "/api/v1/validation", ""); res.status != 200 || len(res.body["files"].([]any)) == 0 {
		t.Errorf("validation: %d %s", res.status, res.raw)
	}
}

// every route the server registers is described in the OpenAPI document, and the document describes nothing else
func TestOpenAPIDescribesEveryRoute(t *testing.T) {
	s, _ := fixture(t, Options{})
	res := call(t, s, "GET", "/api/v1/openapi.json", "")
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal([]byte(res.raw), &doc); err != nil || res.status != 200 {
		t.Fatalf("openapi.json: %d %v", res.status, err)
	}
	var described []string
	for path, ops := range doc.Paths {
		for method := range ops {
			described = append(described, strings.ToUpper(method)+" "+path)
		}
	}
	slices.Sort(described)
	routes := slices.Clone(s.patterns)
	slices.Sort(routes)
	if !slices.Equal(routes, described) {
		t.Errorf("routes and the OpenAPI document differ\n  ROUTES:    %v\n  DESCRIBED: %v", routes, described)
	}
}

func TestEditPreconditions(t *testing.T) {
	s, _ := fixture(t, Options{})
	column := `{"name":"archived","datatype":"boolean","nullable":false,"default":"false"}`
	expect(t, call(t, s, "POST", "/api/v1/state/tables/projects/columns", column), 428, "if_match_required")
	tag := etag(t, s, "projects")
	res := call(t, s, "POST", "/api/v1/state/tables/projects/columns", column, "If-Match", tag)
	if res.status != 201 || res.header.Get("ETag") == "" || res.header.Get("ETag") == tag || !strings.Contains(res.raw, `"name": "archived"`) {
		t.Fatalf("add column: %d %s", res.status, res.raw)
	}
	expect(t, call(t, s, "POST", "/api/v1/state/tables/projects/columns", `{"name":"x","datatype":"text"}`, "If-Match", tag), 412, "stale")
	tag = res.header.Get("ETag")
	expect(t, call(t, s, "POST", "/api/v1/state/tables/projects/columns", `{"name":"x","datatype":"text","nullabel":true}`, "If-Match", tag), 400, "bad_request")
	expect(t, call(t, s, "POST", "/api/v1/state/tables/projects/columns", column, "If-Match", tag), 409, "exists")

	res = call(t, s, "PUT", "/api/v1/state/tables/projects/primary-key", `{"columns":["nope"]}`, "If-Match", tag)
	expect(t, res, 422, "invalid_edit")
	if !strings.Contains(res.raw, "'nope' is not a column") {
		t.Errorf("EXPECTED the issue in the response  ACTUAL: %s", res.raw)
	}
}

func TestCreateAndDropTable(t *testing.T) {
	s, _ := fixture(t, Options{})
	table := `{"name":"notes","columns":[{"name":"id","datatype":"integer","nullable":false}],"primary-key":["id"]}`
	res := call(t, s, "POST", "/api/v1/state/tables", table)
	if res.status != 201 || res.header.Get("ETag") == "" {
		t.Fatalf("create: %d %s", res.status, res.raw)
	}
	expect(t, call(t, s, "POST", "/api/v1/state/tables", table), 409, "exists")
	expect(t, call(t, s, "POST", "/api/v1/state/tables", strings.Replace(table, "notes", "Bad Name", 1)), 400, "invalid_name")
	res = call(t, s, "DELETE", "/api/v1/state/tables/documents", "", "If-Match", etag(t, s, "documents"))
	expect(t, res, 409, "referenced")
	if !strings.Contains(res.raw, "tags.tags_document_fk") {
		t.Errorf("EXPECTED the references listed  ACTUAL: %s", res.raw)
	}
	if res = call(t, s, "DELETE", "/api/v1/state/tables/notes", "", "If-Match", etag(t, s, "notes")); res.status != 200 || res.body["table"] != nil {
		t.Errorf("drop: %d %s", res.status, res.raw)
	}
}

func TestRenameAndBackfillRecordHints(t *testing.T) {
	s, _ := fixture(t, Options{})
	expect(t, call(t, s, "PATCH", "/api/v1/state/tables/documents/columns/file_location", `{"name":"storage_path","comment":"x"}`, "If-Match", etag(t, s, "documents")), 400, "bad_request")
	res := call(t, s, "PATCH", "/api/v1/state/tables/documents/columns/file_location", `{"name":"storage_path"}`, "If-Match", etag(t, s, "documents"))
	if res.status != 200 || !strings.Contains(res.raw, `"to": "storage_path"`) {
		t.Fatalf("rename: %d %s", res.status, res.raw)
	}
	res = call(t, s, "POST", "/api/v1/state/tables/documents/columns", `{"name":"status","datatype":"text","nullable":false,"backfill":{"expression":"'draft'"}}`,
		"If-Match", etag(t, s, "documents"))
	if res.status != 201 {
		t.Fatalf("add with backfill: %d %s", res.status, res.raw)
	}
	expect(t, call(t, s, "POST", "/api/v1/state/tables/documents/columns", `{"name":"x","datatype":"text","backfill":{}}`, "If-Match", etag(t, s, "documents")), 400, "bad_request")

	res = call(t, s, "GET", "/api/v1/changes/hints", "")
	if !strings.Contains(res.raw, `"id": "rename:documents.file_location"`) || !strings.Contains(res.raw, `"id": "backfill:documents.status"`) {
		t.Fatalf("EXPECTED both hints recorded  ACTUAL: %s", res.raw)
	}

	res = call(t, s, "GET", "/api/v1/changes", "")
	for _, want := range []string{`"action": "rename"`, `update documents set status = 'draft' where status is null`, "alter table documents rename column file_location to storage_path"} {
		if res.status != 200 || !strings.Contains(res.raw, want) {
			t.Errorf("changes - EXPECTED %q  ACTUAL: %d %s", want, res.status, res.raw)
		}
	}

	// discarding the rename turns it into a drop and an add, with a likely-rename warning
	if res = call(t, s, "DELETE", "/api/v1/changes/hints/rename:documents.file_location", ""); res.status != 204 {
		t.Errorf("discard hint: %d %s", res.status, res.raw)
	}
	expect(t, call(t, s, "DELETE", "/api/v1/changes/hints/rename:documents.file_location", ""), 404, "not_found")
	if res = call(t, s, "GET", "/api/v1/changes", ""); !strings.Contains(res.raw, "if it is a rename") {
		t.Errorf("EXPECTED a likely-rename warning once the hint is gone  ACTUAL: %s", res.raw)
	}

	// dropping the column forgets its backfill
	call(t, s, "DELETE", "/api/v1/state/tables/documents/columns/status", "", "If-Match", etag(t, s, "documents"))
	if res = call(t, s, "GET", "/api/v1/changes/hints", ""); strings.Contains(res.raw, "backfill:documents.status") {
		t.Errorf("EXPECTED the backfill forgotten with its column  ACTUAL: %s", res.raw)
	}
}

func TestTableObjects(t *testing.T) {
	s, _ := fixture(t, Options{})
	expect(t, call(t, s, "PUT", "/api/v1/state/tables/tags/indexes/tags_label_idx", `{"name":"other","columns":["label"]}`, "If-Match", etag(t, s, "tags")), 400, "bad_request")
	expect(t, call(t, s, "PUT", "/api/v1/state/tables/tags/widgets/x", `{}`, "If-Match", etag(t, s, "tags")), 404, "not_found")
	if res := call(t, s, "PUT", "/api/v1/state/tables/tags/indexes/tags_label_idx", `{"columns":["label"],"unique":true}`, "If-Match", etag(t, s, "tags")); res.status != 200 ||
		!strings.Contains(res.raw, `"name": "tags_label_idx"`) {
		t.Errorf("put index: %d %s", res.status, res.raw)
	}
	if res := call(t, s, "DELETE", "/api/v1/state/tables/tags/indexes/tags_label_idx", "", "If-Match", etag(t, s, "tags")); res.status != 200 {
		t.Errorf("drop index: %d %s", res.status, res.raw)
	}
	expect(t, call(t, s, "DELETE", "/api/v1/state/tables/tags/indexes/tags_label_idx", "", "If-Match", etag(t, s, "tags")), 404, "not_found")
	expect(t, call(t, s, "DELETE", "/api/v1/state/tables/projects/primary-key", "", "If-Match", etag(t, s, "projects")), 409, "referenced")
	if res := call(t, s, "PATCH", "/api/v1/state/tables/projects", `{"comment":"the projects"}`, "If-Match", etag(t, s, "projects")); res.status != 200 ||
		!strings.Contains(res.raw, `"comment": "the projects"`) {
		t.Errorf("table comment: %d %s", res.status, res.raw)
	}
}

func TestBuildChangeset(t *testing.T) {
	s, workdir := fixture(t, Options{})
	expect(t, call(t, s, "POST", "/api/v1/changesets", `{"description":"nothing"}`), 409, "no_changes")
	call(t, s, "POST", "/api/v1/state/tables/projects/columns", `{"name":"archived","datatype":"boolean","nullable":true}`, "If-Match", etag(t, s, "projects"))
	expect(t, call(t, s, "POST", "/api/v1/changesets", `{}`), 400, "bad_request")
	expect(t, call(t, s, "GET", "/api/v1/changes?baseline=no-such-ref", ""), 400, "bad_baseline")
	res := call(t, s, "POST", "/api/v1/changesets", `{"description":"archived projects","bump":"minor"}`)
	if res.status != 201 || res.body["file"] != filepath.Join("deltas", "v1_1_0.json") {
		t.Fatalf("build: %d %s", res.status, res.raw)
	}
	if _, err := os.Stat(filepath.Join(workdir, "deltas", "v1_1_0.json")); err != nil {
		t.Error("EXPECTED the changeset file written")
	}
	expect(t, call(t, s, "POST", "/api/v1/changesets", `{"description":"again"}`), 409, "pending_changeset")
}

func TestCors(t *testing.T) {
	s, _ := fixture(t, Options{CorsOrigin: "http://localhost:5173"})
	req := httptest.NewRequest("OPTIONS", "http://127.0.0.1:7012/api/v1/state/tables/projects/columns", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" || !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "If-Match") {
		t.Errorf("preflight: %d %v", rec.Code, rec.Header())
	}
	req = httptest.NewRequest("GET", "http://127.0.0.1:7012/api/v1/state/tables", nil)
	req.Header.Set("Origin", "http://elsewhere.example.com")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("EXPECTED no CORS headers for another origin")
	}
}

func TestBodyLimit(t *testing.T) {
	s, _ := fixture(t, Options{})
	big := `{"name":"t","columns":[{"name":"c","datatype":"text","comment":"` + string(bytes.Repeat([]byte("x"), maxBody)) + `"}]}`
	expect(t, call(t, s, "POST", "/api/v1/state/tables", big), 413, "too_large")
}

func TestVerifyNeedsADatabase(t *testing.T) {
	s, _ := fixture(t, Options{})
	viper.Set("dbconn.connstring", "")
	expect(t, call(t, s, "POST", "/api/v1/changes/verify", ""), 503, "verify_unavailable")
}
