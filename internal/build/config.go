package build

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/applet-one/cli/internal/project"
)

var (
	identifierPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	jsIdentifierPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
)

// Binding is the project's single Durable Object namespace binding.
type Binding struct {
	Name      string `json:"name"`
	ClassName string `json:"class_name"`
}

// Backup is the optional backup helper configuration.
type Backup struct {
	Binding    string
	ObjectName string
}

// Config is a project configuration validated against the Applet artifact
// v1 subset.
type Config struct {
	Name              string
	Main              string
	CompatibilityDate string
	// CompatibilityFlags and Vars are nil when not configured.
	CompatibilityFlags []string
	Vars               map[string]string
	Binding            Binding
	// Migrations are passed through to the manifest unchanged.
	Migrations []any
	Access     string
	Backup     *Backup
}

// Validate checks a loaded project against the artifact v1 subset.
func Validate(p *project.Project) (*Config, error) {
	c := p.Worker
	allowed := append([]string{"$schema", "name", "main", "compatibility_date", "compatibility_flags", "durable_objects", "migrations", "vars", "applet"}, project.HostingFields...)
	fields := make([]string, 0, len(c))
	for key := range c {
		fields = append(fields, key)
	}
	sort.Strings(fields)
	for _, key := range fields {
		if !slices.Contains(allowed, key) {
			return nil, fmt.Errorf("Unsupported Applet v1 configuration field '%s'. Static assets, build hooks, and other runtime/build features are outside Applet v1.", key)
		}
	}
	if err := project.ValidateName(c["name"]); err != nil {
		return nil, err
	}
	config := &Config{Name: c["name"].(string), Access: p.Access()}

	main, _ := c["main"].(string)
	if strings.TrimSpace(main) == "" {
		return nil, errors.New("Missing or invalid 'main'. Set it to the Worker entry file, for example 'src/index.ts'.")
	}
	config.Main = main

	date, _ := c["compatibility_date"].(string)
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return nil, errors.New("Missing or invalid 'compatibility_date'. Use a real date in YYYY-MM-DD format, for example '2026-09-22'.")
	}
	config.CompatibilityDate = date

	if flags, present := c["compatibility_flags"]; present {
		list, ok := stringList(flags, true)
		if !ok {
			return nil, errors.New("'compatibility_flags' must be an array of strings.")
		}
		config.CompatibilityFlags = list
	}

	if vars, present := c["vars"]; present {
		object, ok := vars.(map[string]any)
		if !ok {
			return nil, errors.New("Applet v1 supports only plain-text string values in 'vars'.")
		}
		config.Vars = map[string]string{}
		for key, value := range object {
			s, ok := value.(string)
			if !ok || !identifierPattern.MatchString(key) {
				return nil, errors.New("Applet v1 supports only plain-text string values in 'vars'.")
			}
			config.Vars[key] = s
		}
	}

	if do, present := c["durable_objects"]; present {
		object, ok := do.(map[string]any)
		if !ok {
			return nil, errors.New("Unsupported durable_objects configuration; Applet v1 supports one namespace binding only.")
		}
		for key := range object {
			if key != "bindings" {
				return nil, errors.New("Unsupported durable_objects configuration; Applet v1 supports one namespace binding only.")
			}
		}
	}
	binding, err := parseBinding(c["durable_objects"])
	if err != nil {
		return nil, err
	}
	config.Binding = binding

	migrations, ok := c["migrations"].([]any)
	if !ok || len(migrations) == 0 {
		return nil, errors.New("Applet v1 requires valid Durable Object 'migrations'.")
	}
	if err := validateMigrations(migrations); err != nil {
		return nil, err
	}
	config.Migrations = migrations

	if err := project.ValidateHosting(p.Hosting); err != nil {
		return nil, err
	}
	if backup, ok := p.Hosting["backup"].(map[string]any); ok {
		config.Backup = &Backup{Binding: backup["binding"].(string), ObjectName: backup["object_name"].(string)}
		if config.Backup.Binding != binding.Name {
			return nil, errors.New("applet.backup must name the single Durable Object binding and a non-empty object_name; its class must be a valid JavaScript identifier.")
		}
	}
	return config, nil
}

func parseBinding(durableObjects any) (Binding, error) {
	invalid := errors.New("Applet projects need exactly one Durable Object binding with a valid 'name' and 'class_name' (for example, STATE and State). Other binding types are not supported yet.")
	object, _ := durableObjects.(map[string]any)
	bindings, ok := object["bindings"].([]any)
	if !ok || len(bindings) != 1 {
		return Binding{}, invalid
	}
	entry, ok := bindings[0].(map[string]any)
	if !ok {
		return Binding{}, invalid
	}
	name, _ := entry["name"].(string)
	className, _ := entry["class_name"].(string)
	if !identifierPattern.MatchString(name) || !jsIdentifierPattern.MatchString(className) {
		return Binding{}, invalid
	}
	for key := range entry {
		if key != "name" && key != "class_name" {
			return Binding{}, fmt.Errorf("Unsupported Durable Object binding field '%s'. Applet v1 supports only a local namespace binding.", key)
		}
	}
	return Binding{Name: name, ClassName: className}, nil
}

func validateMigrations(migrations []any) error {
	tags := map[string]bool{}
	for _, item := range migrations {
		m, ok := item.(map[string]any)
		tag, _ := m["tag"].(string)
		if !ok || tag == "" || tags[tag] {
			return errors.New("Invalid migrations: each migration needs a unique non-empty tag.")
		}
		tags[tag] = true
		fields := make([]string, 0, len(m))
		for key := range m {
			fields = append(fields, key)
		}
		sort.Strings(fields)
		for _, key := range fields {
			value := m[key]
			switch key {
			case "tag":
			case "renamed_classes":
				pairs, ok := value.([]any)
				for _, pair := range pairs {
					p, isObject := pair.(map[string]any)
					_, fromOK := p["from"].(string)
					_, toOK := p["to"].(string)
					ok = ok && isObject && fromOK && toOK
				}
				if !ok {
					return errors.New("Migration renamed_classes must contain { from, to } pairs.")
				}
			case "new_sqlite_classes", "deleted_classes", "new_classes":
				if _, ok := stringList(value, false); !ok {
					return fmt.Errorf("Migration '%s' must be an array of class names.", key)
				}
			default:
				return fmt.Errorf("Unsupported migration field '%s'.", key)
			}
		}
	}
	return nil
}

func stringList(value any, allowEmpty bool) ([]string, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok || (!allowEmpty && s == "") {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
