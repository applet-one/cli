// Package secrets reads and validates app secret values. Values are never
// accepted as arguments or environment variables, and are never echoed.
package secrets

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/applet-one/cli/internal/dotenv"
)

const (
	// MaxValueBytes is the maximum UTF-8 size of one secret value.
	MaxValueBytes = 5 * 1024
	// MaxFileBytes is the maximum size of a deployment secrets file.
	MaxFileBytes = 1024 * 1024
)

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ValidName reports whether name is a valid, unreserved secret name.
func ValidName(name string) bool {
	return namePattern.MatchString(name) && !strings.HasPrefix(name, "APPLET_") && !strings.HasPrefix(name, "__APPLET_")
}

// ValidateValue checks a secret value's size.
func ValidateValue(value string) error {
	if value == "" || len(value) > MaxValueBytes {
		return errors.New("Secret must be non-empty and at most 5 KiB UTF-8.")
	}
	return nil
}

// ReadStdin reads an exact UTF-8 value, including any trailing newline.
func ReadStdin(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxValueBytes+1))
	if err != nil {
		return "", errors.New("Could not read secret stdin.")
	}
	if len(data) > MaxValueBytes {
		return "", errors.New("Secret stdin exceeds 5 KiB UTF-8.")
	}
	if !utf8.Valid(data) {
		return "", errors.New("Secret stdin must be valid UTF-8.")
	}
	value := string(data)
	return value, ValidateValue(value)
}

// Prompt reads a value from the terminal with echo disabled. Ctrl-C and
// Ctrl-D cancel; the terminal state is always restored.
func Prompt(in *os.File, out io.Writer) (string, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("Use applet secrets set NAME --stdin in a non-interactive terminal.")
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", errors.New("Could not read secret input.")
	}
	defer func() {
		_ = term.Restore(fd, state)
		_, _ = io.WriteString(out, "\r\n")
	}()
	_, _ = io.WriteString(out, "Secret value (hidden): ")
	var value []byte
	buffer := make([]byte, 256)
	for {
		n, err := in.Read(buffer)
		if err != nil {
			return "", errors.New("Secret input cancelled.")
		}
		for _, b := range buffer[:n] {
			switch {
			case b == 0x03 || b == 0x04:
				return "", errors.New("Secret input cancelled.")
			case b == '\r' || b == '\n':
				result := string(value)
				if !utf8.ValidString(result) {
					return "", errors.New("Secret input must be valid UTF-8.")
				}
				return result, ValidateValue(result)
			case b == 0x7f || b == '\b':
				if len(value) > 0 {
					_, size := utf8.DecodeLastRune(value)
					value = value[:len(value)-size]
				}
			case b >= ' ' && b != 0x1b:
				value = append(value, b)
			}
			if len(value) > MaxValueBytes {
				return "", errors.New("Secret exceeds 5 KiB UTF-8.")
			}
		}
	}
}

// File is a validated deployment secrets file.
type File struct {
	// Path is the resolved path, which Worker code must not import.
	Path    string
	Secrets map[string]string
}

// Names returns the secret names, sorted.
func (f *File) Names() []string {
	return slices.Sorted(maps.Keys(f.Secrets))
}

// ReadFile reads a JSON string map (.json) or dotenv file. Errors never
// include file contents, which may contain credential material.
func ReadFile(path, dir string) (*File, error) {
	invalid := errors.New("Invalid secrets file. Use a UTF-8 JSON string map or dotenv file, with 1–100 valid names, non-empty values up to 5 KiB each, and a total payload below 1 MiB. No file contents were displayed.")
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, invalid
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, invalid
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return nil, invalid
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil || len(data) > MaxFileBytes || !utf8.Valid(data) {
		return nil, invalid
	}
	values := map[string]string{}
	if strings.EqualFold(filepath.Ext(resolved), ".json") {
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
			return nil, invalid
		}
		for name, value := range raw {
			s, ok := value.(string)
			if !ok {
				return nil, invalid
			}
			values[name] = s
		}
	} else {
		values = dotenv.Parse(string(data))
	}
	if len(values) == 0 || len(values) > 100 {
		return nil, invalid
	}
	for name, value := range values {
		if !ValidName(name) || ValidateValue(value) != nil {
			return nil, invalid
		}
	}
	// Leave room for the hash and envelope in the bounded finalize request.
	if encoded, _ := json.Marshal(values); len(encoded) > MaxFileBytes-1024 {
		return nil, invalid
	}
	return &File{Path: resolved, Secrets: values}, nil
}
