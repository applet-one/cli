package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const legacy = `{
  // Worker settings
  "name": "my-app",
  "main": "src/index.js",
  "applet": {
    // hosting
    "access": "private"
  }
}
`

func TestLoadSplit(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"wrangler.jsonc": `{"name": "my-app", "routes": []}`,
		"applet.jsonc":   `{"access": "private"}`,
	})
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "my-app" || p.Access() != "private" || p.Legacy {
		t.Fatalf("unexpected project: %+v", p)
	}
	if warnings := p.Warnings(); len(warnings) != 1 || !strings.Contains(warnings[0], "routes") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestLoadDefaultsToPublic(t *testing.T) {
	p, err := Load(writeFiles(t, map[string]string{"wrangler.json": `{"name": "my-app"}`}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Access() != "public" || p.AppletExists {
		t.Fatalf("unexpected project: %+v", p)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := map[string]struct {
		files map[string]string
		want  string
	}{
		"both wrangler files": {map[string]string{"wrangler.json": "{}", "wrangler.jsonc": "{}"}, "Both wrangler.jsonc"},
		"toml":                {map[string]string{"wrangler.toml": ""}, "not wrangler.toml"},
		"none":                {map[string]string{}, "No Worker configuration"},
		"applet in wrangler":  {map[string]string{"wrangler.jsonc": `{"name": "a", "applet": {}}`}, "Move Applet hosting"},
		"combined conflict":   {map[string]string{"wrangler.jsonc": `{"name": "a"}`, "applet.jsonc": `{"name": "a"}`}, "combined legacy"},
		"mixed legacy":        {map[string]string{"applet.jsonc": `{"name": "a", "access": "public"}`}, "Mixed legacy"},
		"bad access":          {map[string]string{"wrangler.jsonc": `{"name": "a"}`, "applet.jsonc": `{"access": "secret"}`}, "applet.access"},
		"bad name":            {map[string]string{"wrangler.jsonc": `{"name": "My App"}`}, "Invalid 'name'"},
		"syntax":              {map[string]string{"wrangler.jsonc": "{\n  \"name\": \"a\",,\n}"}, "Invalid wrangler.jsonc at line 2"},
		"duplicate":           {map[string]string{"wrangler.jsonc": `{"name": "a", "name": "b"}`}, "Duplicate configuration field 'name'"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFiles(t, tt.files))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestSetAccessCreatesHostingFile(t *testing.T) {
	dir := writeFiles(t, map[string]string{"wrangler.jsonc": `{"name": "my-app"}`})
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetAccess("private"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "applet.jsonc")); got != "{\n  \"access\": \"private\"\n}\n" {
		t.Fatalf("applet.jsonc = %q", got)
	}
	if got := read(t, filepath.Join(dir, "wrangler.jsonc")); got != `{"name": "my-app"}` {
		t.Fatalf("wrangler.jsonc changed: %q", got)
	}
}

func TestSetAccessLegacyPreservesComments(t *testing.T) {
	dir := writeFiles(t, map[string]string{"applet.jsonc": legacy})
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetAccess("public"); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, filepath.Join(dir, "applet.jsonc")), strings.Replace(legacy, `"private"`, `"public"`, 1); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSetAccessRefusesConcurrentEdit(t *testing.T) {
	dir := writeFiles(t, map[string]string{"wrangler.jsonc": `{"name": "my-app"}`, "applet.jsonc": `{}`})
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "applet.jsonc"), []byte(`{"backup": null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.SetAccess("private"); err == nil || !strings.Contains(err.Error(), "changed during this command") {
		t.Fatalf("err = %v", err)
	}
}

func TestMigrate(t *testing.T) {
	dir := writeFiles(t, map[string]string{"applet.jsonc": legacy})
	result, err := Migrate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated {
		t.Fatal("not migrated")
	}
	if got := read(t, filepath.Join(dir, "applet.legacy.jsonc")); got != legacy {
		t.Fatalf("backup = %q", got)
	}
	if got, want := read(t, filepath.Join(dir, "wrangler.jsonc")), "{\n  // Worker settings\n  \"name\": \"my-app\",\n  \"main\": \"src/index.js\"\n}\n"; got != want {
		t.Fatalf("wrangler.jsonc = %q, want %q", got, want)
	}
	if got, want := read(t, filepath.Join(dir, "applet.jsonc")), "{\n    // hosting\n    \"access\": \"private\"\n  }\n"; got != want {
		t.Fatalf("applet.jsonc = %q, want %q", got, want)
	}
	p, err := Load(dir)
	if err != nil || p.Legacy || p.Access() != "private" {
		t.Fatalf("after migration: %+v, %v", p, err)
	}
	if again, err := Migrate(dir); err != nil || again.Migrated {
		t.Fatalf("second migration: %+v, %v", again, err)
	}
}

func TestMigrateRefusesConflicts(t *testing.T) {
	dir := writeFiles(t, map[string]string{"applet.jsonc": legacy, "applet.legacy.jsonc": "{}"})
	if _, err := Migrate(dir); err == nil || !strings.Contains(err.Error(), "no files were changed") {
		t.Fatalf("err = %v", err)
	}
	if got := read(t, filepath.Join(dir, "applet.jsonc")); got != legacy {
		t.Fatal("applet.jsonc changed")
	}
}
