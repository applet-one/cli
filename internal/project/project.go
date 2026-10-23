// Package project loads, validates and edits Applet project configuration:
// Worker settings in wrangler.jsonc (or wrangler.json) and Applet hosting
// settings in applet.jsonc. Legacy projects keep both in applet.jsonc.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/applet-one/cli/internal/jsonc"
)

// HostingFields are Wrangler fields that Applet ignores because it owns the
// account, hostname and routing.
var HostingFields = []string{"account_id", "zone_id", "route", "routes", "workers_dev", "preview_urls"}

var (
	namePattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Project is a loaded project configuration.
type Project struct {
	Dir string
	// Worker holds Worker fields; Hosting holds Applet hosting settings.
	Worker, Hosting map[string]any
	WorkerFile      string
	AppletFile      string
	AppletText      []byte
	AppletExists    bool
	Legacy          bool
}

// Name is the Worker (and Applet app) name.
func (p *Project) Name() string { return p.Worker["name"].(string) }

// Access is the configured access, defaulting to public.
func (p *Project) Access() string {
	if access, ok := p.Hosting["access"].(string); ok {
		return access
	}
	return "public"
}

// ValidateName checks an app name.
func ValidateName(name any) error {
	if s, ok := name.(string); !ok || !namePattern.MatchString(s) {
		return errors.New("Invalid 'name'. Use 1–63 lowercase letters, numbers, or hyphens, starting with a letter (for example, 'my-app').")
	}
	return nil
}

// ValidateHosting checks Applet hosting settings.
func ValidateHosting(config any) error {
	c, ok := config.(map[string]any)
	if !ok || slices.ContainsFunc(keys(c), func(k string) bool { return !slices.Contains([]string{"$schema", "access", "backup"}, k) }) {
		return errors.New("Applet hosting settings support only '$schema', 'access', and 'backup'. Keep Worker fields in wrangler.jsonc; migrate legacy projects with applet migrate-config.")
	}
	if access, present := c["access"]; present && access != "public" && access != "private" {
		return errors.New("applet.access must be 'public' or 'private'.")
	}
	if backup, present := c["backup"]; present {
		b, ok := backup.(map[string]any)
		binding, _ := b["binding"].(string)
		object, _ := b["object_name"].(string)
		if !ok || slices.ContainsFunc(keys(b), func(k string) bool { return k != "binding" && k != "object_name" }) || !identifierPattern.MatchString(binding) || object == "" {
			return errors.New("applet.backup requires a valid 'binding' and non-empty 'object_name'.")
		}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Load reads the project in dir. Loading intentionally does not validate
// build capabilities: inspection and deletion must work even when the source
// is missing or the project uses unsupported features.
func Load(dir string) (*Project, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	p := &Project{Dir: dir, AppletFile: filepath.Join(dir, "applet.jsonc")}
	var candidates []string
	for _, name := range []string{"wrangler.jsonc", "wrangler.json"} {
		if exists(filepath.Join(dir, name)) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) > 1 {
		return nil, errors.New("Both wrangler.jsonc and wrangler.json exist. Keep one Worker configuration to avoid ambiguity.")
	}
	p.AppletText = []byte("{}\n")
	if text, err := os.ReadFile(p.AppletFile); err == nil {
		p.AppletText, p.AppletExists = text, true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, applet, err := jsonc.Parse(p.AppletText, "applet.jsonc")
	if err != nil {
		return nil, err
	}
	if len(candidates) > 0 {
		p.WorkerFile = filepath.Join(dir, candidates[0])
		text, err := os.ReadFile(p.WorkerFile)
		if err != nil {
			return nil, err
		}
		if _, p.Worker, err = jsonc.Parse(text, candidates[0]); err != nil {
			return nil, err
		}
		if _, ok := p.Worker["applet"]; ok {
			return nil, errors.New("Move Applet hosting settings out of Wrangler configuration into applet.jsonc.")
		}
		if _, ok := applet["name"]; ok {
			return nil, errors.New("A combined legacy applet.jsonc and a Wrangler configuration both exist. Resolve the conflict before migrating; no configuration is selected automatically.")
		}
		if err := ValidateHosting(applet); err != nil {
			return nil, err
		}
		p.Hosting = applet
	} else {
		if _, ok := applet["name"]; !p.AppletExists || !ok {
			if exists(filepath.Join(dir, "wrangler.toml")) {
				return nil, errors.New("Applet currently reads wrangler.jsonc or wrangler.json, not wrangler.toml. Convert the Worker configuration to JSONC first.")
			}
			return nil, errors.New("No Worker configuration found. Add wrangler.jsonc, or use a legacy applet.jsonc project.")
		}
		_, hasAccess := applet["access"]
		_, hasBackup := applet["backup"]
		if hasAccess || hasBackup {
			return nil, errors.New("Mixed legacy and split configuration in applet.jsonc. Use applet migrate-config before moving hosting settings to the top level.")
		}
		hosting, present := applet["applet"]
		if !present {
			hosting = map[string]any{}
		}
		if err := ValidateHosting(hosting); err != nil {
			return nil, err
		}
		p.Hosting = hosting.(map[string]any)
		delete(applet, "applet")
		p.Worker, p.WorkerFile, p.Legacy = applet, p.AppletFile, true
	}
	if err := ValidateName(p.Worker["name"]); err != nil {
		return nil, err
	}
	return p, nil
}

// Warnings returns non-fatal configuration warnings.
func (p *Project) Warnings() []string {
	var warnings []string
	if p.Legacy {
		warnings = append(warnings, "Legacy applet.jsonc Worker configuration: run applet migrate-config to use standard Wrangler configuration.")
	}
	var overridden []string
	for _, key := range HostingFields {
		if _, ok := p.Worker[key]; ok {
			overridden = append(overridden, key)
		}
	}
	if len(overridden) > 0 {
		warnings = append(warnings, fmt.Sprintf("Applet does not use Wrangler hosting fields (%s); it owns the account, generated hostname, and routing. These fields still apply to direct Wrangler deployment.", strings.Join(overridden, ", ")))
	}
	return warnings
}

// HasExplicitAccess reports whether the hosting settings set access.
func (p *Project) HasExplicitAccess() bool {
	_, ok := p.Hosting["access"]
	return ok
}

// SetAccess writes access to applet.jsonc, preserving comments. It refuses to
// overwrite the file if it changed since the project was loaded.
func (p *Project) SetAccess(access string) error {
	doc, _, err := jsonc.Parse(p.AppletText, "applet.jsonc")
	if err != nil {
		return err
	}
	path := []string{"access"}
	if p.Legacy {
		path = []string{"applet", "access"}
	}
	if err := doc.SetString(path, access); err != nil {
		return err
	}
	updated := doc.Bytes()
	_, parsed, err := jsonc.Parse(updated, "applet.jsonc")
	if err != nil {
		return err
	}
	if p.Legacy {
		err = ValidateHosting(parsed["applet"])
	} else {
		err = ValidateHosting(parsed)
	}
	if err != nil {
		return err
	}
	current, readErr := os.ReadFile(p.AppletFile)
	if (readErr == nil) != p.AppletExists || (p.AppletExists && string(current) != string(p.AppletText)) {
		return errors.New("applet.jsonc changed during this command; retry with the updated configuration.")
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(p.AppletFile); err == nil {
		mode = info.Mode().Perm()
	}
	return writeAtomic(p.AppletFile, updated, mode)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// MigrateResult describes a configuration migration.
type MigrateResult struct {
	Migrated               bool
	WorkerFile, BackupFile string
}

// Migrate splits a legacy combined applet.jsonc into wrangler.jsonc and a
// hosting-only applet.jsonc, keeping the original as applet.legacy.jsonc.
func Migrate(dir string) (MigrateResult, error) {
	p, err := Load(dir)
	if err != nil || !p.Legacy {
		return MigrateResult{}, err
	}
	workerFile, backupFile := filepath.Join(p.Dir, "wrangler.jsonc"), filepath.Join(p.Dir, "applet.legacy.jsonc")
	for _, path := range []string{workerFile, backupFile, filepath.Join(p.Dir, "wrangler.json"), filepath.Join(p.Dir, "wrangler.toml")} {
		if exists(path) {
			return MigrateResult{}, errors.New("Migration would overwrite or conflict with an existing Wrangler configuration or applet.legacy.jsonc. Resolve the conflict first; no files were changed.")
		}
	}
	doc, _, err := jsonc.Parse(p.AppletText, "applet.jsonc")
	if err != nil {
		return MigrateResult{}, err
	}
	// Comments inside hosting settings are preserved by moving the raw object.
	hostingText := "{}\n"
	if raw, ok := doc.Raw("applet"); ok {
		hostingText = raw + "\n"
	}
	if err := doc.Remove("applet"); err != nil {
		return MigrateResult{}, err
	}
	workerText := doc.Bytes()
	if _, _, err := jsonc.Parse(workerText, "wrangler.jsonc"); err != nil {
		return MigrateResult{}, err
	}
	_, hosting, err := jsonc.Parse([]byte(hostingText), "applet.jsonc")
	if err != nil {
		return MigrateResult{}, err
	}
	if err := ValidateHosting(hosting); err != nil {
		return MigrateResult{}, err
	}
	info, err := os.Stat(p.AppletFile)
	if err != nil {
		return MigrateResult{}, err
	}
	mode := info.Mode().Perm()
	var created []string
	rollback := func(err error) (MigrateResult, error) {
		for _, path := range created {
			os.Remove(path)
		}
		return MigrateResult{}, err
	}
	if err := writeNew(backupFile, p.AppletText, mode); err != nil {
		return rollback(err)
	}
	created = append(created, backupFile)
	if err := writeNew(workerFile, workerText, mode); err != nil {
		return rollback(err)
	}
	created = append(created, workerFile)
	if current, err := os.ReadFile(p.AppletFile); err != nil || string(current) != string(p.AppletText) {
		return rollback(errors.New("applet.jsonc changed during migration; no configuration was replaced."))
	}
	if err := writeAtomic(p.AppletFile, []byte(hostingText), mode); err != nil {
		return rollback(err)
	}
	return MigrateResult{Migrated: true, WorkerFile: workerFile, BackupFile: backupFile}, nil
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
