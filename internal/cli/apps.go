package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/applet-one/cli/internal/api"
	"github.com/applet-one/cli/internal/project"
)

func newAccessCommand(app *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "access [private|public --yes]",
		Short: "Show or set this project's public/private access",
		Long: `Show or set this project's access, in applet.jsonc and on the live app.

Before the first deployment only applet.jsonc changes. Making an app public
requires --yes.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			usage := errors.New("Usage: applet access [private|public --yes]")
			if (len(args) == 1 && args[0] != "public" && args[0] != "private") || (yes && (len(args) == 0 || args[0] != "public")) {
				return usage
			}
			ctx := cmd.Context()
			p, err := project.Load(app.Dir)
			if err != nil {
				return err
			}
			local := p.Access()
			namespace, err := app.Client.NamespaceSlug(ctx, "No Applet namespace found.")
			if err != nil {
				return err
			}
			remote := ""
			if current, err := app.Client.GetApp(ctx, namespace, p.Name()); err == nil {
				remote = current.Access
			} else if !api.HasCode(err, "NOT_FOUND") {
				return err
			}
			if len(args) == 0 {
				live, note := remote, ""
				if live == "" {
					live = "not deployed"
				} else if remote != local {
					note = " (will be overwritten on deploy)"
				}
				app.printf("Config: %s\nLive: %s%s\n", local, live, note)
				return nil
			}
			desired := args[0]
			if desired == "public" && (local != "public" || remote == "private") && !yes {
				return errors.New("Making an app public requires --yes: applet access public --yes")
			}
			writeConfig := local != desired || !p.HasExplicitAccess()
			// Detect an unwritable config before changing the live setting.
			if writeConfig {
				if err := checkWritable(p); err != nil {
					return err
				}
			}
			if remote != "" && remote != desired {
				if err := app.Client.Do(ctx, http.MethodPatch, api.AppPath(namespace, p.Name())+"/access", map[string]string{"access": desired}, nil); err != nil {
					return err
				}
			}
			if writeConfig {
				if err := p.SetAccess(desired); err != nil {
					retry := "applet access " + desired
					if desired == "public" {
						retry += " --yes"
					}
					return fmt.Errorf("Live access may now be %s, but applet.jsonc was not updated: %v. Retry %s.", desired, err, retry)
				}
			}
			where := " (config only; not deployed)"
			if remote != "" {
				where = " (live and config)"
			}
			app.printf("Access: %s%s\n", desired, where)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm making the app public")
	return cmd
}

func checkWritable(p *project.Project) error {
	if p.AppletExists {
		file, err := os.OpenFile(p.AppletFile, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		file.Close()
	}
	probe, err := os.CreateTemp(filepath.Dir(p.AppletFile), ".applet-write-check-*")
	if err != nil {
		return err
	}
	probe.Close()
	return os.Remove(probe.Name())
}

func newLogsCommand(app *App) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "logs [--limit N]",
		Short: "Show recent deployed app logs",
		Long: `Show the latest deployed app log entries from the last 24 hours, in time order
(not a live tail). Includes deployment outcomes, runtime logs, and dispatch failures.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > 100 {
				return errors.New("Usage: applet logs [--limit N] (N must be 1–100)")
			}
			ctx := cmd.Context()
			p, err := project.Load(app.Dir)
			if err != nil {
				return err
			}
			namespace, err := app.Client.NamespaceSlug(ctx, noNamespace)
			if err != nil {
				return err
			}
			var result struct {
				Items []struct {
					Timestamp string          `json:"timestamp"`
					Source    string          `json:"source"`
					Severity  string          `json:"severity"`
					Message   json.RawMessage `json:"message"`
				} `json:"items"`
				Truncated bool `json:"truncated"`
			}
			if err := app.Client.Do(ctx, http.MethodGet, api.AppPath(namespace, p.Name())+"/logs?limit="+strconv.Itoa(limit), nil, &result); err != nil {
				return err
			}
			if len(result.Items) == 0 {
				app.println("No logs found in the last 24 hours. Redeploy older apps to enable runtime logging; recent events may take time to appear.")
				return nil
			}
			for _, item := range result.Items {
				var message string
				if json.Unmarshal(item.Message, &message) != nil {
					message = string(item.Message)
				}
				message = strings.ReplaceAll(strings.ReplaceAll(message, "\r", ""), "\n", "\n    ")
				app.printf("%s %s: %s: %s\n", item.Timestamp, item.Source, item.Severity, message)
			}
			if result.Truncated {
				app.printf("Showing the latest %d entries. Logs may contain more events.\n", len(result.Items))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "Number of entries to show (1–100)")
	return cmd
}

func newBackupCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Short: "Download this app's configured state backup",
		Long:  "Download this app's configured state backup.\n\nThe project must enable 'backup' in applet.jsonc and be redeployed after enabling it; exports are limited to 10 MiB.",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			p, err := project.Load(app.Dir)
			if err != nil {
				return err
			}
			if _, ok := p.Hosting["backup"]; !ok {
				return errors.New("This project has not enabled backups. Add 'backup' with binding and object_name to applet.jsonc, then redeploy (legacy projects use applet.backup).")
			}
			namespace, err := app.Client.NamespaceSlug(ctx, "No Applet namespace found.")
			if err != nil {
				return err
			}
			var issued struct {
				URL   string `json:"url"`
				Token string `json:"token"`
			}
			if err := app.Client.Do(ctx, http.MethodPost, api.AppPath(namespace, p.Name())+"/backup-token", map[string]any{}, &issued); err != nil {
				return err
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, issued.URL, nil)
			if err != nil {
				return err
			}
			request.Header.Set("authorization", "Bearer "+issued.Token)
			response, err := app.HTTP.Do(request)
			if err != nil {
				return fmt.Errorf("Backup helper failed: %w", err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				return fmt.Errorf("Backup helper failed: %w", err)
			}
			if response.StatusCode < 200 || response.StatusCode > 299 {
				return fmt.Errorf("Backup helper failed (%d): %s", response.StatusCode, body)
			}
			stamp := strings.ReplaceAll(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), ":", "-")
			destination := filepath.Join(app.Dir, p.Name()+"-backup-"+stamp+".json")
			if err := os.WriteFile(destination, body, 0o600); err != nil {
				return err
			}
			app.printf("Backup downloaded to %s\n", destination)
			return nil
		},
	}
}

func newDeleteCommand(app *App) *cobra.Command {
	var yes, noInput bool
	cmd := &cobra.Command{
		Use:   "delete [app-name] [--yes] [--no-input]",
		Short: "Delete a named app from any directory",
		Long: `Permanently delete a hosted app by name, or the app in this project.

Omit app-name to use the current project's Worker name. Without --yes, you are
asked to type the app name; --no-input disables prompting and requires --yes.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if noInput && !yes {
				return errors.New("Deletion requires confirmation. Re-run with --yes to confirm non-interactively.")
			}
			var name string
			if len(args) == 1 {
				name = args[0]
				if !appNamePattern.MatchString(name) {
					return errors.New("App names must start with a lowercase letter and contain only lowercase letters, numbers, and hyphens.")
				}
			} else {
				p, err := project.Load(app.Dir)
				if err != nil {
					return err
				}
				name = p.Name()
			}
			if !yes {
				if !app.IsTerminal() {
					return errors.New("Refusing to delete without confirmation in a non-interactive terminal. Re-run with --yes.")
				}
				confirmation, err := app.readLine(fmt.Sprintf("Type %s to permanently delete its hosted deployment: ", name))
				if err != nil || confirmation != name {
					return errors.New("Deletion cancelled: confirmation did not match the app name.")
				}
			}
			ctx := cmd.Context()
			namespace, err := app.Client.NamespaceSlug(ctx, noNamespace)
			if err != nil {
				return err
			}
			app.progress("Deleting %s...", name)
			var result struct {
				Operation api.Operation `json:"operation"`
			}
			if err := app.Client.Do(ctx, http.MethodDelete, api.AppPath(namespace, name), map[string]string{"confirmation": name}, &result, api.IdempotencyKey(api.NewIdempotencyKey())); err != nil {
				return err
			}
			for range 60 {
				if err := app.Sleep(ctx, 2*time.Second); err != nil {
					return err
				}
				var current struct {
					Operation api.Operation `json:"operation"`
				}
				if err := app.Client.Do(ctx, http.MethodGet, "/operations/"+api.PathEscape(result.Operation.ID), nil, &current); err != nil {
					return err
				}
				switch current.Operation.Status {
				case "succeeded":
					app.printf("Deleted: %s\n", name)
					return nil
				case "failed":
					if current.Operation.ErrorMessage != nil {
						return errors.New(*current.Operation.ErrorMessage)
					}
					return errors.New("Deletion failed.")
				}
			}
			app.printf("Deletion is still processing. Check the dashboard for operation %s.\n", result.Operation.ID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm deletion; required in non-interactive use")
	cmd.Flags().BoolVar(&noInput, "no-input", false, "Do not prompt; requires --yes")
	return cmd
}
