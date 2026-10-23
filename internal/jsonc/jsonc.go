// Package jsonc parses JSON with comments and trailing commas, and edits it
// while preserving comments and formatting.
package jsonc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/tailscale/hujson"
)

// Document is a parsed JSONC object.
type Document struct {
	root hujson.Value
	eol  string
}

var positionPattern = regexp.MustCompile(`^hujson: line (\d+), column (\d+): (.*)$`)

// Parse parses text as a JSONC object. Duplicate keys at any depth are
// rejected because they make edits ambiguous.
func Parse(text []byte, filename string) (*Document, map[string]any, error) {
	root, err := hujson.Parse(text)
	if err != nil {
		if m := positionPattern.FindStringSubmatch(err.Error()); m != nil {
			return nil, nil, fmt.Errorf("Invalid %s at line %s, column %s: %s.", filename, m[1], m[2], m[3])
		}
		return nil, nil, fmt.Errorf("Invalid %s: %v.", filename, err)
	}
	if _, ok := root.Value.(*hujson.Object); !ok {
		return nil, nil, fmt.Errorf("%s must contain a JSON object.", filename)
	}
	if err := checkDuplicates(&root, filename); err != nil {
		return nil, nil, err
	}
	standard := root.Clone()
	standard.Standardize()
	var value map[string]any
	if err := json.Unmarshal(standard.Pack(), &value); err != nil {
		return nil, nil, fmt.Errorf("Invalid %s: %v.", filename, err)
	}
	eol := "\n"
	if bytes.Contains(text, []byte("\r\n")) {
		eol = "\r\n"
	}
	return &Document{root: root, eol: eol}, value, nil
}

func checkDuplicates(v *hujson.Value, filename string) error {
	switch value := v.Value.(type) {
	case *hujson.Object:
		seen := map[string]bool{}
		for i := range value.Members {
			name := value.Members[i].Name.Value.(hujson.Literal).String()
			if seen[name] {
				return fmt.Errorf("Duplicate configuration field '%s' in %s.", name, filename)
			}
			seen[name] = true
			if err := checkDuplicates(&value.Members[i].Value, filename); err != nil {
				return err
			}
		}
	case *hujson.Array:
		for i := range value.Elements {
			if err := checkDuplicates(&value.Elements[i], filename); err != nil {
				return err
			}
		}
	}
	return nil
}

// Bytes returns the document text.
func (d *Document) Bytes() []byte { return d.root.Pack() }

// Raw returns the exact text of the value at a top-level key, including any
// comments inside it, or false if the key does not exist.
func (d *Document) Raw(key string) (string, bool) {
	member := findMember(d.root.Value.(*hujson.Object), key)
	if member == nil {
		return "", false
	}
	return hujson.Value{Value: member.Value.Value}.String(), true
}

// Remove deletes a top-level key, preserving the rest of the document.
func (d *Document) Remove(key string) error {
	if d.root.Find("/"+key) == nil {
		return nil
	}
	return d.root.Patch(fmt.Appendf(nil, `[{"op":"remove","path":%q}]`, "/"+key))
}

// SetString sets the string value at an object path, creating intermediate
// objects as needed.
func (d *Document) SetString(path []string, value string) error {
	obj, indent := d.root.Value.(*hujson.Object), ""
	for i, key := range path {
		member := findMember(obj, key)
		last := i == len(path)-1
		if member == nil {
			var newValue hujson.ValueTrimmed = hujson.String(value)
			if !last {
				newValue = &hujson.Object{}
			}
			member = d.appendMember(obj, key, newValue, indent)
		} else if last {
			member.Value.Value = hujson.String(value)
		}
		if last {
			break
		}
		next, ok := member.Value.Value.(*hujson.Object)
		if !ok {
			return fmt.Errorf("'%s' must be an object", strings.Join(path[:i+1], "."))
		}
		indent = memberIndent(obj, indent)
		obj = next
	}
	d.root.UpdateOffsets()
	return nil
}

func findMember(obj *hujson.Object, key string) *hujson.ObjectMember {
	for i := range obj.Members {
		if obj.Members[i].Name.Value.(hujson.Literal).String() == key {
			return &obj.Members[i]
		}
	}
	return nil
}

// memberIndent is the indentation used for members of obj, whose closing
// brace is indented by parentIndent.
func memberIndent(obj *hujson.Object, parentIndent string) string {
	if len(obj.Members) > 0 {
		before := string(obj.Members[0].Name.BeforeExtra)
		if i := strings.LastIndex(before, "\n"); i >= 0 && strings.TrimLeft(before[i+1:], " \t") == "" {
			return before[i+1:]
		}
	}
	return parentIndent + "  "
}

func (d *Document) appendMember(obj *hujson.Object, key string, value hujson.ValueTrimmed, parentIndent string) *hujson.ObjectMember {
	inline := len(obj.Members) > 0 && !bytes.Contains(obj.Members[0].Name.BeforeExtra, []byte("\n"))
	member := hujson.ObjectMember{
		Name:  hujson.Value{Value: hujson.String(key)},
		Value: hujson.Value{BeforeExtra: hujson.Extra(" "), Value: value},
	}
	if inline {
		member.Name.BeforeExtra = hujson.Extra(" ")
	} else {
		indent := memberIndent(obj, parentIndent)
		// Comments after the previous last member stay with it.
		kept := strings.TrimRight(string(obj.AfterExtra), " \t\r\n")
		member.Name.BeforeExtra = hujson.Extra(kept + d.eol + indent)
		obj.AfterExtra = hujson.Extra(d.eol + parentIndent)
	}
	if n := len(obj.Members); n > 0 && obj.Members[n-1].Value.AfterExtra != nil {
		member.Value.AfterExtra = hujson.Extra{}
	}
	obj.Members = append(obj.Members, member)
	return &obj.Members[len(obj.Members)-1]
}
