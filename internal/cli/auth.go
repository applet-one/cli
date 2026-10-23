package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/applet-one/cli/internal/api"
	"github.com/applet-one/cli/internal/auth"
)

func newLoginCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Sign in to Applet",
		Long:  "Sign in to Applet with browser-based device authorization.",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			deviceName := os.Getenv("APPLET_DEVICE_NAME")
			if deviceName == "" {
				deviceName = runtime.GOOS + " CLI"
			}
			var authorization struct {
				VerificationURI string `json:"verification_uri"`
				UserCode        string `json:"user_code"`
				DeviceCode      string `json:"device_code"`
				ExpiresIn       int    `json:"expires_in"`
				Interval        int    `json:"interval"`
			}
			err := app.Client.Do(ctx, http.MethodPost, "/cli/device/init", map[string]any{
				"device_name": deviceName,
				"scopes":      []string{"apps:read", "apps:write", "deployments:write"},
			}, &authorization, api.NoAuth())
			if err != nil {
				return err
			}
			app.printf("Open %s and enter code: %s\n", authorization.VerificationURI, authorization.UserCode)
			app.OpenBrowser(authorization.VerificationURI)
			interval := max(time.Duration(authorization.Interval)*time.Second, time.Second)
			deadline := time.Now().Add(time.Duration(authorization.ExpiresIn) * time.Second)
			for time.Now().Before(deadline) {
				if err := app.Sleep(ctx, interval); err != nil {
					return err
				}
				var token struct {
					AccessToken string `json:"access_token"`
					ExpiresIn   int    `json:"expires_in"`
				}
				err := app.Client.Do(ctx, http.MethodPost, "/cli/device/token", map[string]string{"device_code": authorization.DeviceCode}, &token, api.NoAuth())
				if api.HasCode(err, "AUTHORIZATION_PENDING") {
					continue
				}
				if err != nil {
					return err
				}
				if err := app.Store.Save(token.AccessToken, time.Duration(token.ExpiresIn)*time.Second); err != nil {
					return err
				}
				app.println("Signed in. Token stored with owner-only file permissions.")
				return nil
			}
			return errors.New("Device authorization expired.")
		},
	}
}

func newLogoutCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign out and remove the saved login",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			if err := app.Store.Remove(); err != nil {
				return err
			}
			app.println("Signed out.")
			return nil
		},
	}
}

func newStatusCommand(app *App) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [--json]",
		Short: "Check sign-in status",
		Long:  "Show sign-in status and deployed apps.",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			writeJSON := func(value any) error {
				return json.NewEncoder(app.Stdout).Encode(value)
			}
			if _, err := app.Store.Token(); err != nil {
				if errors.Is(err, auth.ErrExpired) {
					if asJSON {
						return writeJSON(map[string]any{"signed_in": false, "reason": "expired"})
					}
					app.println("Stored token is expired.")
					return nil
				}
				if asJSON {
					return writeJSON(map[string]any{"signed_in": false, "reason": "not_signed_in"})
				}
				app.println("Not signed in.")
				return nil
			}
			failed := func(err error) error {
				app.progress("applet: Unable to fetch app status: %s", err)
				return exitError{1}
			}
			ctx := cmd.Context()
			namespace, raw, ok, err := app.Client.FirstNamespace(ctx)
			if err != nil {
				return failed(err)
			}
			if !ok {
				if asJSON {
					return writeJSON(map[string]any{"signed_in": true, "namespace": nil, "apps": []any{}})
				}
				app.println("Signed in. No Applet namespace found.")
				return nil
			}
			var result struct {
				Items []json.RawMessage `json:"items"`
			}
			if err := app.Client.Do(ctx, http.MethodGet, "/namespaces/"+api.PathEscape(namespace.Slug)+"/apps", nil, &result); err != nil {
				return failed(err)
			}
			if result.Items == nil {
				result.Items = []json.RawMessage{}
			}
			if asJSON {
				return writeJSON(map[string]any{"signed_in": true, "namespace": raw, "apps": result.Items})
			}
			switch {
			case namespace.AppLimit != nil:
				app.printf("Signed in. %d/%d apps.\n", len(result.Items), *namespace.AppLimit)
			case len(result.Items) == 1:
				app.println("Signed in. 1 app.")
			default:
				app.printf("Signed in. %d apps.\n", len(result.Items))
			}
			for _, item := range result.Items {
				var a api.App
				if err := json.Unmarshal(item, &a); err != nil {
					return failed(err)
				}
				private := ""
				if a.Access == "private" {
					private = " [private]"
				}
				app.printf("  %s (%s) — %s%s\n", a.Name, a.Status, a.URL, private)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print a stable JSON document to stdout")
	return cmd
}
