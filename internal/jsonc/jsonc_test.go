package jsonc

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, text string) *Document {
	t.Helper()
	doc, _, err := Parse([]byte(text), "applet.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestParseAcceptsCommentsAndTrailingCommas(t *testing.T) {
	_, value, err := Parse([]byte("{\n  // comment\n  \"name\": \"app\", /* block */\n  \"list\": [1, 2,],\n}\n"), "wrangler.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	if value["name"] != "app" {
		t.Fatalf("name = %v", value["name"])
	}
}

func TestParseErrors(t *testing.T) {
	for name, text := range map[string]string{
		"syntax":     "{\n \"a\": 1,,\n}",
		"not object": "[1]",
		"duplicate":  `{"a": {"b": 1, "b": 2}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Parse([]byte(text), "applet.jsonc")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "applet.jsonc") {
				t.Fatalf("error does not name the file: %v", err)
			}
		})
	}
}

func TestSetString(t *testing.T) {
	tests := []struct {
		name, in, want string
		path           []string
	}{
		{"replace", "{\n  \"access\": \"public\", // keep\n  \"x\": 1\n}\n", "{\n  \"access\": \"private\", // keep\n  \"x\": 1\n}\n", []string{"access"}},
		{"append", "{\n  // c\n  \"backup\": {}\n}\n", "{\n  // c\n  \"backup\": {},\n  \"access\": \"private\"\n}\n", []string{"access"}},
		{"append after comment", "{\n  \"a\": 1 // note\n}\n", "{\n  \"a\": 1, // note\n  \"access\": \"private\"\n}\n", []string{"access"}},
		{"empty", "{}\n", "{\n  \"access\": \"private\"\n}\n", []string{"access"}},
		{"trailing comma", "{\n  \"a\": 1,\n}\n", "{\n  \"a\": 1,\n  \"access\": \"private\",\n}\n", []string{"access"}},
		{"inline", "{ \"a\": 1 }", "{ \"a\": 1, \"access\": \"private\" }", []string{"access"}},
		{"nested new", "{\n  \"name\": \"x\"\n}\n", "{\n  \"name\": \"x\",\n  \"applet\": {\n    \"access\": \"private\"\n  }\n}\n", []string{"applet", "access"}},
		{"nested existing", "{\n\t\"applet\": {\n\t\t\"backup\": {}\n\t}\n}\n", "{\n\t\"applet\": {\n\t\t\"backup\": {},\n\t\t\"access\": \"private\"\n\t}\n}\n", []string{"applet", "access"}},
		{"crlf", "{\r\n  \"a\": 1\r\n}\r\n", "{\r\n  \"a\": 1,\r\n  \"access\": \"private\"\r\n}\r\n", []string{"access"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mustParse(t, tt.in)
			if err := doc.SetString(tt.path, "private"); err != nil {
				t.Fatal(err)
			}
			if got := string(doc.Bytes()); got != tt.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tt.want)
			}
			if _, _, err := Parse(doc.Bytes(), "out"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRemoveAndRaw(t *testing.T) {
	doc := mustParse(t, "{\n  \"name\": \"x\", // n\n  \"applet\": {\n    // hosting\n    \"access\": \"private\"\n  },\n  \"main\": \"a\"\n}\n")
	raw, ok := doc.Raw("applet")
	if !ok || raw != "{\n    // hosting\n    \"access\": \"private\"\n  }" {
		t.Fatalf("raw = %q", raw)
	}
	if err := doc.Remove("applet"); err != nil {
		t.Fatal(err)
	}
	if got, want := string(doc.Bytes()), "{\n  \"name\": \"x\", // n\n  \"main\": \"a\"\n}\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, ok := doc.Raw("applet"); ok {
		t.Fatal("applet still present")
	}
}
