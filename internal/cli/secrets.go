package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/applet-one/cli/internal/api"
	"github.com/applet-one/cli/internal/project"
	"github.com/applet-one/cli/internal/secrets"
)

const secretsDocs = "Docs: https://applet.one/dashboard/docs/secrets"

func newSecretsCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Set, list, or delete deployed app credentials",
		Long: `Manage this deployed app's write-only credentials.

Run from the project directory after a successful deployment; code reads env.NAME.
Values are never arguments or environment variables. Use printf, not echo.
Code publication requests preservation of current secrets; .dev.vars is local-only.
On uncertain outcomes, retry deliberately; delayed retries can overwrite a rotation.

` + secretsDocs,
		Args: usageArgs(cobra.NoArgs),
	}

	var setStdin, setNoInput, setJSON bool
	set := &cobra.Command{
		Use:   "set NAME [--stdin] [--no-input] [--json]",
		Short: "Set or replace a deployed app credential",
		Long: `Set or replace a deployed app credential; values are write-only.

Without --stdin, a TTY is required and input echo is disabled.
Deploy code that safely handles a missing env.NAME first, then set the secret.
Never put values in arguments, environment variables, configuration or logs.

` + secretsDocs,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := checkSecretName(name); err != nil {
				return err
			}
			if !setStdin && (setNoInput || !app.IsTerminal()) {
				return errors.New("Use applet secrets set NAME --stdin; --no-input disables prompting.")
			}
			endpoint, err := app.secretsEndpoint(cmd.Context())
			if err != nil {
				return err
			}
			var value string
			if setStdin {
				value, err = secrets.ReadStdin(app.Stdin)
			} else {
				value, err = secrets.Prompt(os.Stdin, app.Stderr)
			}
			if err != nil {
				return err
			}
			if err := app.secretRequest(cmd.Context(), http.MethodPut, endpoint+"/"+api.PathEscape(name), map[string]string{"value": value}, nil); err != nil {
				return err
			}
			return app.acknowledgeSecret(setJSON, "Set", name)
		},
	}
	set.Flags().BoolVar(&setStdin, "stdin", false, "Read exact UTF-8 stdin, including any trailing newline (max 5 KiB)")
	set.Flags().BoolVar(&setNoInput, "no-input", false, "Disable hidden prompting; requires --stdin")
	set.Flags().BoolVar(&setJSON, "json", false, "Print name-only JSON acknowledgement")

	var listJSON bool
	list := &cobra.Command{
		Use:   "list [--json]",
		Short: "List this deployed app's secret names (never values)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			endpoint, err := app.secretsEndpoint(cmd.Context())
			if err != nil {
				return err
			}
			type item struct {
				Name string `json:"name"`
				Type string `json:"type"`
			}
			var result struct {
				Items []item `json:"items"`
			}
			if err := app.secretRequest(cmd.Context(), http.MethodGet, endpoint, nil, &result); err != nil {
				return err
			}
			items := result.Items
			if items == nil {
				items = []item{}
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
			if listJSON {
				return json.NewEncoder(app.Stdout).Encode(map[string]any{"items": items})
			}
			if len(items) == 0 {
				app.println("No secrets configured.")
			}
			for _, item := range items {
				app.println(item.Name)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "Print sorted names/types as JSON")

	var deleteYes, deleteNoInput, deleteJSON bool
	remove := &cobra.Command{
		Use:   "delete NAME [--yes] [--no-input] [--json]",
		Short: "Delete a deployed app credential after confirmation",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := checkSecretName(name); err != nil {
				return err
			}
			if !deleteYes && (deleteNoInput || !app.IsTerminal()) {
				return errors.New("Noninteractive deletion requires applet secrets delete NAME --yes.")
			}
			endpoint, err := app.secretsEndpoint(cmd.Context())
			if err != nil {
				return err
			}
			if !deleteYes {
				confirmation, err := app.readLine(fmt.Sprintf("Type %s to delete this secret: ", name))
				if err != nil || confirmation != name {
					return errors.New("Secret deletion cancelled.")
				}
			}
			if err := app.secretRequest(cmd.Context(), http.MethodDelete, endpoint+"/"+api.PathEscape(name), nil, nil); err != nil {
				return err
			}
			return app.acknowledgeSecret(deleteJSON, "Deleted", name)
		},
	}
	remove.Flags().BoolVar(&deleteYes, "yes", false, "Confirm deletion; required without a TTY")
	remove.Flags().BoolVar(&deleteNoInput, "no-input", false, "Do not prompt; requires --yes")
	remove.Flags().BoolVar(&deleteJSON, "json", false, "Print name-only JSON acknowledgement")

	cmd.AddCommand(set, list, remove)
	return cmd
}

func checkSecretName(name string) error {
	if !secrets.ValidName(name) {
		return errors.New("Invalid or reserved secret name.")
	}
	return nil
}

func (a *App) secretsEndpoint(ctx context.Context) (string, error) {
	p, err := project.Load(a.Dir)
	if err != nil {
		return "", err
	}
	namespace, err := a.Client.NamespaceSlug(ctx, "No Applet namespace found.")
	if err != nil {
		return "", err
	}
	return api.AppPath(namespace, p.Name()) + "/secrets", nil
}

// secretRequest never displays an untrusted response message, which might
// reflect a value.
func (a *App) secretRequest(ctx context.Context, method, path string, body, out any) error {
	err := a.Client.Do(ctx, method, path, body, out, api.Timeout(20*time.Second))
	if err == nil {
		return nil
	}
	code := api.Code(err)
	if !slices.Contains([]string{"APP_NOT_DEPLOYED", "APP_BUSY", "BINDING_NAME_CONFLICT", "NOT_FOUND", "RATE_LIMITED", "INVALID_SECRET_VALUE", "INVALID_SECRET_NAME", "SECRETS_UNAVAILABLE"}, code) {
		code = "SECRET_OUTCOME_UNCERTAIN"
	}
	advice := "The outcome may be uncertain. Retry deliberately with the intended current value; a delayed retry can overwrite a newer rotation."
	if method == http.MethodGet {
		advice = "Try again later."
	}
	return fmt.Errorf("%s: Secret operation did not complete normally. %s", code, advice)
}

func (a *App) acknowledgeSecret(asJSON bool, verb, name string) error {
	if asJSON {
		return json.NewEncoder(a.Stdout).Encode(map[string]string{"name": name})
	}
	a.printf("%s: %s\n", verb, name)
	return nil
}
