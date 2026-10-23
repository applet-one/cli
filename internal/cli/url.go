package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/applet-one/cli/internal/api"
	"github.com/applet-one/cli/internal/project"
)

const urlWarning = "The previous URL will stop working and its name can immediately be used by another app. Returning visitors may expose browser data stored for the old address to its new owner. Browser storage and sign-in do not transfer. Update links, webhooks, and authentication callbacks."

var (
	// Generated canonical labels can be longer than the custom-name policy;
	// they are accepted provisionally for an unchanged-name no-op.
	labelPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	publicNamePattern = regexp.MustCompile(`^[a-z]{3,}(?:-[a-z]{3,})+$`)
	errInvalidName    = errors.New("INVALID_PUBLIC_NAME: Use two or more words of at least 3 ASCII letters each, separated by hyphens (maximum 32 characters).")
)

type publicName struct {
	Name                  string         `json:"name"`
	URL                   string         `json:"url"`
	CustomAppNamesEnabled bool           `json:"custom_app_names_enabled"`
	PendingOperation      *api.Operation `json:"pending_operation"`
	OperationID           string         `json:"operation_id"`
}

func validCustomName(name string) bool {
	return len(name) <= 32 && publicNamePattern.MatchString(name)
}

func normalizePublicName(arg string, custom bool) (string, error) {
	name := strings.ToLower(strings.TrimSpace(arg))
	if len(arg) > 128 || !labelPattern.MatchString(name) || (custom && !validCustomName(name)) {
		return "", errInvalidName
	}
	return name, nil
}

func newURLCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "url",
		Short: "Show or change this hosted app's public URL",
		Long: `Show or explicitly change this hosted app's public URL (Pro).

Names are claimed only when saved. Old names are immediately reusable.
Does not change project files, app identity, or deployment settings.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, current, err := app.publicName(cmd.Context())
			if err != nil {
				return err
			}
			app.println(current.URL)
			if op := current.PendingOperation; op != nil {
				app.printf("Updating URL: operation %s (%s). Run applet url again to inspect progress.\n", op.ID, op.Phase)
			}
			return nil
		},
	}

	check := &cobra.Command{
		Use:   "check NAME",
		Short: "Check whether a name is available; claims nothing",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := normalizePublicName(args[0], true)
			if err != nil {
				return err
			}
			endpoint, _, err := app.publicName(cmd.Context())
			if err != nil {
				return err
			}
			var result struct {
				Name      string `json:"name"`
				Available bool   `json:"available"`
			}
			if err := app.Client.Do(cmd.Context(), http.MethodGet, endpoint+"/availability?name="+url.QueryEscape(name), nil, &result, api.Timeout(30*time.Second)); err != nil {
				return err
			}
			if !result.Available {
				app.printf("%s: unavailable\n", result.Name)
				return errors.New("PUBLIC_NAME_UNAVAILABLE: This name is unavailable.")
			}
			app.printf("%s: available (claimed only when saved)\n", result.Name)
			return nil
		},
	}

	var yes bool
	set := &cobra.Command{
		Use:   "set NAME [--yes]",
		Short: "Change the public URL",
		Long: `Change this hosted app's public URL.

` + urlWarning + `

--yes is required without an interactive terminal.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := normalizePublicName(args[0], false)
			if err != nil {
				return err
			}
			return app.setPublicName(cmd.Context(), name, yes)
		},
	}
	set.Flags().BoolVar(&yes, "yes", false, "Confirm the change without prompting")

	cmd.AddCommand(check, set)
	return cmd
}

func (a *App) publicName(ctx context.Context) (string, publicName, error) {
	var current publicName
	p, err := project.Load(a.Dir)
	if err != nil {
		return "", current, err
	}
	namespace, err := a.Client.NamespaceSlug(ctx, "No Applet namespace found.")
	if err != nil {
		return "", current, err
	}
	endpoint := api.AppPath(namespace, p.Name()) + "/public-name"
	if err := a.Client.Do(ctx, http.MethodGet, endpoint, nil, &current, api.Timeout(30*time.Second)); err != nil {
		if api.HasCode(err, "NOT_FOUND") {
			return "", current, errors.New("No remotely created app found. Deploy/create this project before choosing its URL.")
		}
		return "", current, err
	}
	return endpoint, current, nil
}

func (a *App) setPublicName(ctx context.Context, name string, yes bool) error {
	endpoint, current, err := a.publicName(ctx)
	if err != nil {
		return err
	}
	if current.PendingOperation != nil {
		return fmt.Errorf("URL change is already pending: operation %s. Run applet url to inspect progress.", current.PendingOperation.ID)
	}
	if name == current.Name {
		a.println(current.URL)
		return nil
	}
	if !validCustomName(name) {
		return errInvalidName
	}
	if !current.CustomAppNamesEnabled {
		return errors.New("CUSTOM_APP_NAMES_DISABLED: Contact us to request custom app URLs.")
	}
	a.printf("Old URL: %s\nNew URL: https://%s.applet.works\n%s\n", current.URL, name, urlWarning)
	if !yes {
		if !a.IsTerminal() {
			return errors.New("Changing the URL requires --yes without an interactive terminal: applet url set NAME --yes")
		}
		answer, err := a.readLine("Change URL? [y/N] ")
		if err != nil || strings.ToLower(strings.TrimSpace(answer)) != "y" {
			return errors.New("URL change cancelled; no request was submitted.")
		}
	}

	key := api.NewIdempotencyKey()
	var result publicName
	err = a.Client.Do(ctx, http.MethodPut, endpoint, map[string]string{"name": name, "expected_current_name": current.Name}, &result, api.IdempotencyKey(key), api.Timeout(30*time.Second))
	if err != nil {
		prefix := ""
		if code := api.Code(err); code != "" {
			prefix = code + ": "
		}
		return fmt.Errorf("%s%v Inspect applet url before retrying; transport failure does not imply rollback. Idempotency key: %s.", prefix, err, key)
	}
	if result.PendingOperation == nil {
		a.println(result.URL)
		return nil
	}
	operationID := result.OperationID
	if operationID == "" {
		operationID = result.PendingOperation.ID
	}
	a.printf("Updating URL… Operation %s; run applet url to inspect progress. Closing this client does not roll back the server operation.\n", operationID)
	deadline := time.Now().Add(time.Minute)
	for attempt := 0; attempt < 30 && time.Now().Before(deadline); attempt++ {
		if err := a.Sleep(ctx, 2*time.Second); err != nil {
			return fmt.Errorf("%v Operation %s; run applet url to inspect progress. Cancellation does not roll back the server operation.", err, operationID)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		var state struct {
			URL       string        `json:"url"`
			Operation api.Operation `json:"operation"`
		}
		if err := a.Client.Do(ctx, http.MethodGet, endpoint+"/operations/"+api.PathEscape(operationID), nil, &state, api.Timeout(min(30*time.Second, remaining))); err != nil {
			return fmt.Errorf("%v Operation %s; run applet url to inspect progress. Cancellation does not roll back the server operation.", err, operationID)
		}
		switch state.Operation.Phase {
		case "completed":
			a.println(state.URL)
			return nil
		case "cancelled":
			return fmt.Errorf("The URL operation was cancelled. Operation %s; run applet url to inspect progress. Cancellation does not roll back the server operation.", operationID)
		}
	}
	return fmt.Errorf("URL update is incomplete. Operation %s; run applet url to inspect progress. Do not assume the server rolled back.", operationID)
}
