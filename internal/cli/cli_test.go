package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applet-one/cli/internal/api"
	"github.com/applet-one/cli/internal/auth"
)

// fakeAPI records requests and serves canned responses keyed by "METHOD path".
type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	requests []recorded
	routes   map[string]func(body map[string]any) (int, any)
	server   *httptest.Server
}

type recorded struct {
	Method, Path string
	Header       http.Header
	Body         map[string]any
	Raw          []byte
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, routes: map[string]func(map[string]any) (int, any){}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.requests = append(f.requests, recorded{r.Method, r.URL.RequestURI(), r.Header.Clone(), body, raw})
		route := f.routes[r.Method+" "+r.URL.RequestURI()]
		f.mu.Unlock()
		if route == nil {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status, response := route(body)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) handle(route string, status int, data any) {
	f.routes[route] = func(map[string]any) (int, any) { return status, map[string]any{"data": data} }
}

func (f *fakeAPI) fail(route string, status int, code string) {
	f.routes[route] = func(map[string]any) (int, any) {
		return status, map[string]any{"error": map[string]string{"code": code, "message": "failed: " + code}}
	}
}

func (f *fakeAPI) find(method, path string) *recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.requests {
		if f.requests[i].Method == method && f.requests[i].Path == path {
			return &f.requests[i]
		}
	}
	return nil
}

type harness struct {
	app            *App
	api            *fakeAPI
	stdout, stderr *bytes.Buffer
	dir            string
}

func newHarness(t *testing.T, signedIn bool) *harness {
	t.Helper()
	f := newFakeAPI(t)
	f.handle("GET /me", 200, map[string]any{"namespaces": []any{map[string]any{"slug": "acme", "app_limit": 5}}})
	store := &auth.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	if signedIn {
		if err := store.Save("token-123", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{api: f, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, dir: t.TempDir()}
	h.app = &App{
		Stdin:       strings.NewReader(""),
		Stdout:      h.stdout,
		Stderr:      h.stderr,
		Dir:         h.dir,
		Store:       store,
		Client:      &api.Client{BaseURL: f.server.URL, Token: store.Token},
		HTTP:        f.server.Client(),
		Sleep:       func(context.Context, time.Duration) error { return nil },
		IsTerminal:  func() bool { return false },
		OpenBrowser: func(string) {},
	}
	return h
}

func (h *harness) run(t *testing.T, args ...string) int {
	t.Helper()
	t.Setenv("APPLET_NO_UPDATE_CHECK", "1")
	return Execute(context.Background(), h.app, args)
}

func (h *harness) writeStarter(t *testing.T) {
	t.Helper()
	for name, content := range starterFiles("my-app") {
		path := filepath.Join(h.dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *harness) deployRoutes() {
	app := map[string]any{"app": map[string]any{"id": "app-1", "name": "my-app", "access": "public", "url": "https://bright-otter.applet.works"}}
	h.api.handle("POST /namespaces/acme/apps", 201, map[string]any{})
	h.api.handle("GET /namespaces/acme/apps/my-app", 200, app)
	h.api.handle("PATCH /namespaces/acme/apps/my-app/access", 200, map[string]any{})
	h.api.handle("POST /namespaces/acme/apps/my-app/deployments", 201, map[string]any{
		"deployment": map[string]any{"id": "dep-1", "status": "pending"},
		"upload":     map[string]any{"url": h.api.server.URL + "/upload/dep-1", "method": "PUT"},
	})
	h.api.handle("PUT /upload/dep-1", 200, map[string]any{})
	h.api.handle("POST /namespaces/acme/apps/my-app/deployments/dep-1/finalize", 200, map[string]any{"deployment": map[string]any{"id": "dep-1", "status": "queued"}})
	h.api.handle("GET /namespaces/acme/apps/my-app/deployments/dep-1", 200, map[string]any{"deployment": map[string]any{"id": "dep-1", "status": "live"}})
}

func TestDeploy(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	h.deployRoutes()
	if code := h.run(t, "deploy"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if got := h.stdout.String(); got != "Deployed: https://bright-otter.applet.works\n" {
		t.Fatalf("stdout = %q", got)
	}
	create := h.api.find("POST", "/namespaces/acme/apps")
	if create.Body["name"] != "my-app" || create.Body["access"] != "public" || create.Header.Get("idempotency-key") == "" {
		t.Fatalf("create = %+v", create)
	}
	if auth := create.Header.Get("authorization"); auth != "Bearer token-123" {
		t.Fatalf("authorization = %q", auth)
	}
	upload := h.api.find("PUT", "/upload/dep-1")
	if upload.Header.Get("content-type") != "application/gzip" || upload.Header.Get("authorization") != "" {
		t.Fatalf("upload headers = %v", upload.Header)
	}
	finalize := h.api.find("POST", "/namespaces/acme/apps/my-app/deployments/dep-1/finalize")
	if _, ok := finalize.Body["secrets"]; ok || len(finalize.Body["sha256"].(string)) != 64 {
		t.Fatalf("finalize = %v", finalize.Body)
	}
}

func TestDeployWithSecretsFile(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	h.deployRoutes()
	h.api.handle("POST /namespaces/acme/apps/my-app/deployments/dep-1/finalize", 200, map[string]any{"deployment": map[string]any{"id": "dep-1", "status": "live"}})
	secretsPath := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsPath, []byte("API_KEY='p@$$word'\nOTHER=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run(t, "deploy", "--secrets-file", secretsPath); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	finalize := h.api.find("POST", "/namespaces/acme/apps/my-app/deployments/dep-1/finalize")
	secrets, _ := finalize.Body["secrets"].(map[string]any)
	if secrets["API_KEY"] != "p@$$word" || secrets["OTHER"] != "two" {
		t.Fatalf("secrets = %v", finalize.Body["secrets"])
	}
	if !strings.Contains(h.stderr.String(), "Publishing Worker with 2 secrets: API_KEY, OTHER") {
		t.Fatalf("stderr = %q", h.stderr)
	}
	if strings.Contains(h.stdout.String()+h.stderr.String(), "p@$$word") {
		t.Fatal("secret value printed")
	}
}

func TestDeploySecretsUncertainOutcome(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	h.deployRoutes()
	h.api.fail("POST /namespaces/acme/apps/my-app/deployments/dep-1/finalize", 500, "SOMETHING_ELSE")
	secretsPath := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(secretsPath, []byte(`{"API_KEY": "value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run(t, "deploy", "--secrets-file", secretsPath); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.stderr.String(), "applet: SECRET_OUTCOME_UNCERTAIN:") {
		t.Fatalf("stderr = %q", h.stderr)
	}
}

func TestDeployRejectsInvalidSecretsFileWithoutLeakingContents(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	secretsPath := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(secretsPath, []byte(`{"APPLET_TOKEN": "hunter2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run(t, "deploy", "--secrets-file", secretsPath); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.stderr.String(), "Invalid secrets file") || strings.Contains(h.stderr.String(), "hunter2") {
		t.Fatalf("stderr = %q", h.stderr)
	}
}

func TestStatus(t *testing.T) {
	h := newHarness(t, true)
	h.api.handle("GET /namespaces/acme/apps", 200, map[string]any{"items": []any{
		map[string]any{"name": "my-app", "status": "live", "url": "https://a.applet.works", "access": "private"},
	}})
	if code := h.run(t, "status"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if want := "Signed in. 1/5 apps.\n  my-app (live) — https://a.applet.works [private]\n"; h.stdout.String() != want {
		t.Fatalf("stdout = %q", h.stdout)
	}
	h.stdout.Reset()
	if code := h.run(t, "status", "--json"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var result map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["signed_in"] != true || result["namespace"].(map[string]any)["slug"] != "acme" || len(result["apps"].([]any)) != 1 {
		t.Fatalf("json = %v", result)
	}
}

func TestStatusSignedOut(t *testing.T) {
	h := newHarness(t, false)
	if code := h.run(t, "status", "--json"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := h.stdout.String(); got != `{"reason":"not_signed_in","signed_in":false}`+"\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestSecretsSetFromStdin(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	h.app.Stdin = strings.NewReader("line\n")
	h.api.handle("PUT /namespaces/acme/apps/my-app/secrets/API_KEY", 200, map[string]any{})
	if code := h.run(t, "secrets", "set", "API_KEY", "--stdin", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if got := h.api.find("PUT", "/namespaces/acme/apps/my-app/secrets/API_KEY").Body["value"]; got != "line\n" {
		t.Fatalf("value = %q", got)
	}
	if h.stdout.String() != `{"name":"API_KEY"}`+"\n" {
		t.Fatalf("stdout = %q", h.stdout)
	}
}

func TestSecretsErrorsHideServerMessages(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	h.app.Stdin = strings.NewReader("value")
	h.api.fail("PUT /namespaces/acme/apps/my-app/secrets/API_KEY", 400, "WEIRD")
	if code := h.run(t, "secrets", "set", "API_KEY", "--stdin"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := h.stderr.String(); !strings.HasPrefix(got, "applet: SECRET_OUTCOME_UNCERTAIN:") || strings.Contains(got, "failed: WEIRD") {
		t.Fatalf("stderr = %q", got)
	}
}

func TestSecretsRequireStdinWithoutTerminal(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	if code := h.run(t, "secrets", "set", "API_KEY"); code != 1 || !strings.Contains(h.stderr.String(), "--stdin") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if len(h.api.requests) != 0 {
		t.Fatal("made requests before validating input")
	}
}

func TestSecretsList(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	h.api.handle("GET /namespaces/acme/apps/my-app/secrets", 200, map[string]any{"items": []any{
		map[string]any{"name": "ZED", "type": "secret_text"}, map[string]any{"name": "ALPHA", "type": "secret_text"},
	}})
	if code := h.run(t, "secrets", "list"); code != 0 || h.stdout.String() != "ALPHA\nZED\n" {
		t.Fatalf("exit %d: %q", code, h.stdout)
	}
}

func TestAccessPrivateBeforeDeploy(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	h.api.fail("GET /namespaces/acme/apps/my-app", 404, "NOT_FOUND")
	if code := h.run(t, "access", "private"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if h.stdout.String() != "Access: private (config only; not deployed)\n" {
		t.Fatalf("stdout = %q", h.stdout)
	}
	data, _ := os.ReadFile(filepath.Join(h.dir, "applet.jsonc"))
	if !strings.Contains(string(data), `"access": "private"`) {
		t.Fatalf("applet.jsonc = %s", data)
	}
}

func TestAccessPublicRequiresYes(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	h.api.handle("GET /namespaces/acme/apps/my-app", 200, map[string]any{"app": map[string]any{"access": "private"}})
	if code := h.run(t, "access", "public"); code != 1 || !strings.Contains(h.stderr.String(), "requires --yes") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	h.stderr.Reset()
	h.api.handle("PATCH /namespaces/acme/apps/my-app/access", 200, map[string]any{})
	if code := h.run(t, "access", "public", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if h.api.find("PATCH", "/namespaces/acme/apps/my-app/access").Body["access"] != "public" {
		t.Fatal("live access not changed")
	}
}

func TestDelete(t *testing.T) {
	h := newHarness(t, true)
	h.api.handle("DELETE /namespaces/acme/apps/other-app", 202, map[string]any{"operation": map[string]any{"id": "op-1"}})
	h.api.handle("GET /operations/op-1", 200, map[string]any{"operation": map[string]any{"id": "op-1", "status": "succeeded"}})
	if code := h.run(t, "delete", "other-app"); code != 1 || !strings.Contains(h.stderr.String(), "non-interactive") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if code := h.run(t, "delete", "other-app", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if h.api.find("DELETE", "/namespaces/acme/apps/other-app").Body["confirmation"] != "other-app" || h.stdout.String() != "Deleted: other-app\n" {
		t.Fatalf("stdout = %q", h.stdout)
	}
}

func TestDeleteInteractiveConfirmation(t *testing.T) {
	h := newHarness(t, true)
	h.app.IsTerminal = func() bool { return true }
	h.app.Stdin = strings.NewReader("wrong\n")
	if code := h.run(t, "delete", "other-app"); code != 1 || !strings.Contains(h.stderr.String(), "did not match") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}

func TestLogs(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	h.api.handle("GET /namespaces/acme/apps/my-app/logs?limit=2", 200, map[string]any{"items": []any{
		map[string]any{"timestamp": "t1", "source": "runtime", "severity": "info", "message": "a\r\nb"},
		map[string]any{"timestamp": "t2", "source": "deployment", "severity": "info", "message": map[string]any{"k": 1}},
	}, "truncated": true})
	if code := h.run(t, "logs", "--limit", "2"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	want := "t1 runtime: info: a\n    b\nt2 deployment: info: {\"k\":1}\nShowing the latest 2 entries. Logs may contain more events.\n"
	if h.stdout.String() != want {
		t.Fatalf("stdout = %q", h.stdout)
	}
}

func TestURLSet(t *testing.T) {
	h := newHarness(t, true)
	h.writeStarter(t)
	endpoint := "/namespaces/acme/apps/my-app/public-name"
	h.api.handle("GET "+endpoint, 200, map[string]any{"name": "bright-otter", "url": "https://bright-otter.applet.works", "custom_app_names_enabled": true})
	h.api.handle("PUT "+endpoint, 202, map[string]any{"pending_operation": map[string]any{"id": "op-9", "phase": "pending"}})
	h.api.handle("GET "+endpoint+"/operations/op-9", 200, map[string]any{"url": "https://sunny-weather.applet.works", "operation": map[string]any{"phase": "completed"}})
	if code := h.run(t, "url", "set", "sunny-weather"); code != 1 || !strings.Contains(h.stderr.String(), "requires --yes") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	h.stdout.Reset()
	if code := h.run(t, "url", "set", "Sunny-Weather", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	put := h.api.find("PUT", endpoint)
	if put.Body["name"] != "sunny-weather" || put.Body["expected_current_name"] != "bright-otter" || put.Header.Get("idempotency-key") == "" {
		t.Fatalf("put = %+v", put)
	}
	if !strings.HasSuffix(h.stdout.String(), "https://sunny-weather.applet.works\n") {
		t.Fatalf("stdout = %q", h.stdout)
	}
}

func TestInit(t *testing.T) {
	h := newHarness(t, false)
	if code := h.run(t, "init", "my-app"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "my-app", "src", "index.js")); err != nil {
		t.Fatal(err)
	}
	if code := h.run(t, "init", "my-app"); code != 1 || !strings.Contains(h.stderr.String(), "already exists") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	h.stderr.Reset()
	if code := h.run(t, "init", "Bad_Name"); code != 1 || !strings.Contains(h.stderr.String(), "Project names") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}

func TestInitCurrentDirectoryRefusesConflicts(t *testing.T) {
	h := newHarness(t, false)
	h.app.Dir = filepath.Join(h.dir, "demo-app")
	if err := os.MkdirAll(h.app.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.app.Dir, "wrangler.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := h.run(t, "init", "."); code != 1 || !strings.Contains(h.stderr.String(), "wrangler.toml") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.app.Dir, "package.json")); err == nil {
		t.Fatal("files were written despite a conflict")
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"status", "extra"}, {"logs", "--limit", "0"}, {"build"}, {"access", "secret"}, {"nope"}} {
		h := newHarness(t, false)
		if code := h.run(t, args...); code != 1 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestPnpmSeparator(t *testing.T) {
	h := newHarness(t, false)
	if code := h.run(t, "--", "version"); code != 0 || h.stdout.String() != "dev\n" {
		t.Fatalf("exit %d: %q", code, h.stdout)
	}
}
