package dev

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapnpString(t *testing.T) {
	got := capnpString("a\"b\\c\nd\x01é")
	if want := `"a\"b\\c\nd\x01é"`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestLoadDevVars(t *testing.T) {
	dir := t.TempDir()
	if values, err := LoadDevVars(dir, nil); err != nil || len(values) != 0 {
		t.Fatalf("missing file: %v %v", values, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".dev.vars"), []byte("TOKEN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if values, err := LoadDevVars(dir, nil); err != nil || values["TOKEN"] != "abc" {
		t.Fatalf("got %v %v", values, err)
	}
	if _, err := LoadDevVars(dir, []string{"TOKEN"}); err == nil {
		t.Fatal("expected binding collision error")
	}
}

func TestCheckAddressInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := CheckAddress("127.0.0.1", port); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v", err)
	}
}

// TestRunIntegration downloads workerd and serves a Durable Object project.
func TestRunIntegration(t *testing.T) {
	if os.Getenv("APPLET_INTEGRATION") == "" {
		t.Skip("set APPLET_INTEGRATION=1 to run workerd")
	}
	dir := t.TempDir()
	files := map[string]string{
		"wrangler.jsonc": `{
  "name": "it-app",
  "main": "src/index.js",
  "compatibility_date": "2026-09-22",
  "vars": { "GREETING": "hello" },
  "durable_objects": { "bindings": [{ "name": "STATE", "class_name": "State" }] },
  "migrations": [{ "tag": "v1", "new_sqlite_classes": ["State"] }]
}`,
		"src/index.js": `export class State {
  constructor(ctx) { this.ctx = ctx; }
  fetch() {
    const sql = this.ctx.storage.sql;
    sql.exec("CREATE TABLE IF NOT EXISTS c (v INTEGER NOT NULL)");
    sql.exec("INSERT INTO c (v) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM c)");
    return Response.json({ count: sql.exec("UPDATE c SET v = v + 1 RETURNING v").one().v });
  }
}
export default {
  async fetch(request, env) {
    const { count } = await env.STATE.get(env.STATE.idFromName("x")).fetch(request).then(r => r.json());
    return Response.json({ count, greeting: env.GREETING, secret: env.SECRET });
  },
};
`,
		".dev.vars": "SECRET=local\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Dir: dir, Host: "127.0.0.1", Port: port, Stdout: io.Discard, Stderr: os.Stderr, Ready: func(url string, _ []string) { ready <- url }})
	}()
	var url string
	select {
	case url = <-ready:
	case err := <-done:
		t.Fatalf("Run: %v", err)
	}
	for want := 1; want <= 2; want++ {
		response, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Count    int    `json:"count"`
			Greeting string `json:"greeting"`
			Secret   string `json:"secret"`
		}
		err = json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if body.Count != want || body.Greeting != "hello" || body.Secret != "local" {
			t.Fatalf("response %d = %+v", want, body)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".applet", "dev", "durable-objects", "it-app-State")); err != nil {
		t.Fatalf("state not persisted: %v", err)
	}
}
