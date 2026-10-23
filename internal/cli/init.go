package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/applet-one/cli/internal/project"
)

var appNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func newInitCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "init <project-name|.>",
		Short: "Create a starter project",
		Long:  "Create a starter Applet Worker project.\n\nPass . to scaffold the current directory; its folder name becomes the app name.",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(_ *cobra.Command, args []string) error {
			directory := args[0]
			if strings.HasPrefix(directory, "-") {
				return errors.New("Usage: applet init <project-name|.>")
			}
			inCurrent := directory == "."
			target := filepath.Join(app.Dir, directory)
			if filepath.IsAbs(directory) {
				target = filepath.Clean(directory)
			}
			name := filepath.Base(target)
			if !appNamePattern.MatchString(name) {
				return errors.New("Project names must start with a lowercase letter and contain only lowercase letters, numbers, and hyphens.")
			}
			if _, err := os.Stat(target); !inCurrent && err == nil {
				return fmt.Errorf("Cannot create %s: the directory already exists.", target)
			}
			files := starterFiles(name)
			if inCurrent {
				var conflicts []string
				for _, path := range append(slices.Sorted(maps.Keys(files)), "wrangler.json", "wrangler.toml") {
					if _, err := os.Lstat(filepath.Join(target, path)); err == nil {
						conflicts = append(conflicts, path)
					}
				}
				if info, err := os.Stat(filepath.Join(target, "src")); err == nil && !info.IsDir() {
					conflicts = append(conflicts, "src/")
				}
				if len(conflicts) > 0 {
					return fmt.Errorf("Cannot initialize this directory because these paths already exist: %s. Move or rename them first; no files were changed.", strings.Join(conflicts, ", "))
				}
			}
			for _, path := range slices.Sorted(maps.Keys(files)) {
				destination := filepath.Join(target, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(destination, []byte(files[path]), 0o644); err != nil {
					return err
				}
			}
			app.printf("Created %s in %s\n", name, target)
			if inCurrent {
				app.println("Next: applet dev")
			} else {
				app.printf("Next: cd %s && applet dev\n", directory)
			}
			return nil
		},
	}
}

func jsonFile(value any) string {
	data, _ := json.MarshalIndent(value, "", "  ")
	return string(data) + "\n"
}

type orderedConfig struct {
	Name              string `json:"name"`
	Main              string `json:"main"`
	CompatibilityDate string `json:"compatibility_date"`
	DurableObjects    any    `json:"durable_objects"`
	Migrations        any    `json:"migrations"`
}

func starterFiles(name string) map[string]string {
	return map[string]string{
		".gitignore":          "node_modules\n.wrangler\n.applet\n.applet-build\ndev.vars\n.dev.vars\n.dev.vars.*\n",
		"pnpm-workspace.yaml": "packages: []\n",
		"package.json": jsonFile(struct {
			Name    string            `json:"name"`
			Private bool              `json:"private"`
			Version string            `json:"version"`
			Type    string            `json:"type"`
			Scripts map[string]string `json:"scripts"`
		}{name, true, "0.0.0", "module", map[string]string{"deploy": "applet deploy", "dev": "applet dev"}}),
		"wrangler.jsonc": jsonFile(orderedConfig{
			Name:              name,
			Main:              "src/index.js",
			CompatibilityDate: "2026-09-22",
			DurableObjects: map[string]any{"bindings": []any{struct {
				Name      string `json:"name"`
				ClassName string `json:"class_name"`
			}{"APPLET_STATE", "AppletState"}}},
			Migrations: []any{struct {
				Tag              string   `json:"tag"`
				NewSQLiteClasses []string `json:"new_sqlite_classes"`
			}{"v1", []string{"AppletState"}}},
		}),
		"applet.jsonc": jsonFile(struct {
			Access string            `json:"access"`
			Backup map[string]string `json:"backup"`
		}{"public", map[string]string{"binding": "APPLET_STATE", "object_name": "default"}}),
		"src/index.js": `export class AppletState {
  constructor(ctx) {
    this.ctx = ctx;
  }

  async fetch() {
    this.ctx.storage.sql.exec("CREATE TABLE IF NOT EXISTS counters (value INTEGER NOT NULL)");
    this.ctx.storage.sql.exec("INSERT INTO counters (value) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM counters)");
    const row = this.ctx.storage.sql.exec("UPDATE counters SET value = value + 1 RETURNING value").one();
    return Response.json({ count: row.value });
  }
}

export default {
  async fetch(request, env) {
    const id = env.APPLET_STATE.idFromName("default");
    return env.APPLET_STATE.get(id).fetch(request);
  },
};
`,
		"README.md": "# " + name + `

A Cloudflare Worker created with Applet. Applet builds and deploys it independently of Wrangler; local development runs on workerd, the open-source Workers runtime. Wrangler is optional for direct deployment to your Cloudflare account.

## Run locally

` + "```sh\napplet dev\n```" + `

Local Durable Object state is kept in ` + "`.applet/dev`" + `. npm dependencies are bundled when you build, so run ` + "`pnpm install`" + ` after adding any.

## Deploy with Applet

` + "```sh\napplet deploy\n```" + `

## Deploy directly with Wrangler

Install Wrangler as an optional development dependency and use your own Cloudflare credentials:

` + "```sh\npnpm exec wrangler deploy\n```" + `

## Delete the Applet deployment

` + "```sh\napplet delete\n```" + `

This permanently deletes the hosted deployment; it does not delete your local project.

## App access

The starter is public by default. Run ` + "`applet access private`" + ` to make the deployed app available only to your signed-in Applet account. Run ` + "`applet access public --yes`" + ` to reopen it. Worker configuration lives in ` + "`wrangler.jsonc`" + `; Applet-only hosting settings live in ` + "`applet.jsonc`" + `. Deployments always reapply ` + "`access`" + ` from ` + "`applet.jsonc`" + `, even after a dashboard change. Direct Wrangler deployment does not enforce Applet private access or enable managed backups.

## Back up application state

Backups are enabled for this starter. After deploying, run ` + "`applet backup`" + ` to download a JSON backup of its state.
`,
	}
}

func newMigrateConfigCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate-config",
		Short: "Split legacy Worker and hosting configuration",
		Long:  "Split a legacy combined applet.jsonc into wrangler.jsonc and Applet-only hosting settings.\n\nPreserves the original as applet.legacy.jsonc; refuses conflicting Wrangler files. Local only; no login or deployment needed.",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			result, err := project.Migrate(app.Dir)
			if err != nil {
				return err
			}
			if result.Migrated {
				app.printf("Migrated Worker configuration to %s. Hosting settings remain in applet.jsonc. Original saved as %s.\n", result.WorkerFile, result.BackupFile)
			} else {
				app.println("Project already uses separate Worker and hosting configuration; no files changed.")
			}
			return nil
		},
	}
}
