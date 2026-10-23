package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const worker = `export class State {}
export default { fetch() { return new Response("ok"); } };
`

func newProject(t *testing.T, wrangler string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"wrangler.jsonc": wrangler, "src/index.ts": worker}
	for name, content := range extra {
		files[name] = content
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
	return dir
}

const validConfig = `{
  "name": "my-app",
  "main": "src/index.ts",
  "compatibility_date": "2026-09-22",
  "vars": { "LABEL": "x" },
  "durable_objects": { "bindings": [{ "name": "STATE", "class_name": "State" }] },
  "migrations": [{ "tag": "v1", "new_sqlite_classes": ["State"] }],
}`

func readArtifact(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	compressed, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(compressed)
	files := map[string][]byte{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg || header.Mode != 0o644 || !header.ModTime.Equal(header.ModTime.Truncate(0)) {
			t.Fatalf("unexpected header %+v", header)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = content
	}
	return files
}

func TestBuildArtifact(t *testing.T) {
	result, err := Build(context.Background(), Options{Dir: newProject(t, validConfig, nil)})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := result.Artifact("app-1")
	if err != nil {
		t.Fatal(err)
	}
	files := readArtifact(t, artifact.Bytes)
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	var manifest Manifest
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.MainModule != "modules/worker.mjs" || manifest.Vars["LABEL"] != "x" || manifest.Bindings[0].ClassName != "State" || len(manifest.Migrations) != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	sum := sha256.Sum256(files["modules/worker.mjs"])
	if manifest.Modules[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("module hash mismatch")
	}
	again, err := result.Artifact("app-1")
	if err != nil {
		t.Fatal(err)
	}
	if again.SHA256() != artifact.SHA256() {
		t.Fatal("artifact is not reproducible")
	}
}

func TestBuildBackupWrapper(t *testing.T) {
	dir := newProject(t, validConfig, map[string]string{"applet.jsonc": `{"backup": {"binding": "STATE", "object_name": "main"}}`})
	result, err := Build(context.Background(), Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Artifact(""); err == nil {
		t.Fatal("expected an error without an app id")
	}
	artifact, err := result.Artifact("app-1")
	if err != nil {
		t.Fatal(err)
	}
	files := readArtifact(t, artifact.Bytes)
	wrapper := string(files["modules/worker.mjs"])
	for _, want := range []string{`import userDefault, { State as UserObject } from "./user-worker.mjs"`, `claims.app_id!=="app-1"`, `export class State extends UserObject`, `idFromName("main")`, `env["STATE"]`} {
		if !strings.Contains(wrapper, want) {
			t.Errorf("wrapper missing %q", want)
		}
	}
	if _, ok := files["modules/user-worker.mjs"]; !ok {
		t.Fatal("missing user module")
	}
}

func TestBuildRejectsSecretsFileImport(t *testing.T) {
	dir := newProject(t, validConfig, map[string]string{
		"src/index.ts": `import secrets from "../secrets.json";` + "\n" + worker + "console.log(secrets);\n",
		"secrets.json": `{"KEY": "value"}`,
	})
	forbidden, err := filepath.EvalSymlinks(filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(context.Background(), Options{Dir: dir, ForbiddenFile: forbidden})
	if err == nil || !strings.Contains(err.Error(), "Secrets files must not be imported") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildValidation(t *testing.T) {
	base := map[string]any{}
	if err := json.Unmarshal([]byte(strings.Replace(validConfig, "}],\n}", "}]\n}", 1)), &base); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		change func(map[string]any)
		want   string
	}{
		"unknown field":   {func(c map[string]any) { c["assets"] = map[string]any{} }, "Unsupported Applet v1 configuration field 'assets'"},
		"missing main":    {func(c map[string]any) { delete(c, "main") }, "Missing or invalid 'main'"},
		"outside project": {func(c map[string]any) { c["main"] = "../index.ts" }, "inside the project"},
		"not a module":    {func(c map[string]any) { c["main"] = "src/index.css" }, "JavaScript or TypeScript"},
		"invalid date":    {func(c map[string]any) { c["compatibility_date"] = "2026-02-30" }, "compatibility_date"},
		"flags":           {func(c map[string]any) { c["compatibility_flags"] = []any{1} }, "compatibility_flags"},
		"vars":            {func(c map[string]any) { c["vars"] = map[string]any{"A": 1} }, "plain-text string values"},
		"two bindings": {func(c map[string]any) {
			c["durable_objects"] = map[string]any{"bindings": []any{map[string]any{"name": "A", "class_name": "A"}, map[string]any{"name": "B", "class_name": "B"}}}
		}, "exactly one Durable Object binding"},
		"binding field": {func(c map[string]any) {
			c["durable_objects"] = map[string]any{"bindings": []any{map[string]any{"name": "STATE", "class_name": "State", "script_name": "x"}}}
		}, "Unsupported Durable Object binding field 'script_name'"},
		"no migrations": {func(c map[string]any) { c["migrations"] = []any{} }, "requires valid Durable Object 'migrations'"},
		"duplicate tag": {func(c map[string]any) {
			c["migrations"] = []any{map[string]any{"tag": "v1"}, map[string]any{"tag": "v1"}}
		}, "unique non-empty tag"},
		"migration field": {func(c map[string]any) { c["migrations"] = []any{map[string]any{"tag": "v1", "x": 1}} }, "Unsupported migration field 'x'"},
		"renamed classes": {func(c map[string]any) {
			c["migrations"] = []any{map[string]any{"tag": "v1", "renamed_classes": []any{"A"}}}
		}, "{ from, to }"},
		"missing class": {func(c map[string]any) {
			c["durable_objects"] = map[string]any{"bindings": []any{map[string]any{"name": "STATE", "class_name": "Other"}}}
		}, "export the configured Durable Object class 'Other'"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			config := map[string]any{}
			data, _ := json.Marshal(base)
			_ = json.Unmarshal(data, &config)
			tt.change(config)
			data, _ = json.Marshal(config)
			_, err := Build(context.Background(), Options{Dir: newProject(t, string(data), nil)})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBuildReportsSyntaxErrors(t *testing.T) {
	dir := newProject(t, validConfig, map[string]string{"src/index.ts": "export default {"})
	_, err := Build(context.Background(), Options{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "src/index.ts:1:") {
		t.Fatalf("err = %v", err)
	}
}
