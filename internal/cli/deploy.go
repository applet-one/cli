package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/applet-one/cli/internal/api"
	"github.com/applet-one/cli/internal/build"
	"github.com/applet-one/cli/internal/dev"
	"github.com/applet-one/cli/internal/secrets"
)

const noNamespace = "No Applet namespace found. Complete onboarding in the dashboard first."

func newBuildCommand(app *App) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "build --dry-run",
		Short: "Validate and summarize a local artifact",
		Long:  "Validate and build an Applet artifact without uploading.",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !dryRun {
				return errors.New("Usage: applet build --dry-run")
			}
			result, err := build.Build(cmd.Context(), build.Options{Dir: app.Dir})
			if err != nil {
				return err
			}
			app.warn(result.Warnings)
			artifact, err := result.Artifact("dry-run")
			if err != nil {
				return err
			}
			m := artifact.Manifest
			summary, err := json.MarshalIndent(struct {
				Valid         bool                    `json:"valid"`
				FormatVersion int                     `json:"format_version"`
				MainModule    string                  `json:"main_module"`
				Modules       []build.ManifestModule  `json:"modules"`
				Bindings      []build.ManifestBinding `json:"bindings"`
				Migrations    []any                   `json:"migrations"`
				ArtifactBytes int                     `json:"artifact_bytes"`
			}{true, m.FormatVersion, m.MainModule, m.Modules, m.Bindings, m.Migrations, len(artifact.Bytes)}, "", "  ")
			if err != nil {
				return err
			}
			app.println(string(summary))
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Build without uploading (required)")
	return cmd
}

func newDevCommand(app *App) *cobra.Command {
	var host string
	var port int
	cmd := &cobra.Command{
		Use:   "dev [--host HOST] [--port PORT]",
		Short: "Run the Worker locally",
		Long:  "Run the Applet Worker locally on workerd, the open-source Workers runtime.\n\nLocal Durable Object state persists under .applet/dev. Restart the command after source changes.\nThe runtime is downloaded on first use; set APPLET_WORKERD to use your own workerd binary.",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if host == "" || strings.HasPrefix(host, "-") {
				return errors.New("--host needs a host name or IP address, for example --host 127.0.0.1.")
			}
			if port < 1 || port > 65535 {
				return errors.New("--port must be from 1 to 65535.")
			}
			return dev.Run(cmd.Context(), dev.Options{
				Dir:    app.Dir,
				Host:   host,
				Port:   port,
				Stdout: app.Stdout,
				Stderr: app.Stderr,
				Ready: func(url string, warnings []string) {
					app.warn(warnings)
					app.printf("Local Worker ready: %s\n", url)
					app.println("Press Ctrl-C to stop.")
				},
			})
		},
	}
	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "Address to listen on")
	cmd.Flags().IntVar(&port, "port", 8787, "Port to listen on (1–65535)")
	return cmd
}

func newDeployCommand(app *App) *cobra.Command {
	var secretsFile string
	cmd := &cobra.Command{
		Use:   "deploy [--secrets-file PATH]",
		Short: "Build and deploy, optionally with credentials",
		Long: `Build and deploy the Applet project in the current directory.

--secrets-file takes a UTF-8 JSON string map or dotenv file and publishes code
and the supplied secrets together, including on the first deployment. Values
never enter artifacts or queues. Keep the file protected and out of Git.
Omitted secrets are preserved; supplied names replace current values. There are
no automatic retries; verify the integration before retrying uncertain outcomes.

Docs: https://applet.one/dashboard/docs/secrets`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("secrets-file") && (secretsFile == "" || strings.HasPrefix(secretsFile, "-")) {
				return errors.New("Usage: applet deploy [--secrets-file PATH]")
			}
			return app.deploy(cmd.Context(), secretsFile)
		},
	}
	cmd.Flags().StringVar(&secretsFile, "secrets-file", "", "Publish secrets from a JSON string map or dotenv file")
	return cmd
}

func (a *App) deploy(ctx context.Context, secretsFile string) error {
	var supplied *secrets.File
	if secretsFile != "" {
		var err error
		if supplied, err = secrets.ReadFile(secretsFile, a.Dir); err != nil {
			return err
		}
	}
	a.progress("Building Worker...")
	options := build.Options{Dir: a.Dir}
	if supplied != nil {
		options.ForbiddenFile = supplied.Path
	}
	result, err := build.Build(ctx, options)
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("Deployment cancelled.")
		}
		return err
	}
	config := result.Config
	if supplied != nil {
		for name := range supplied.Secrets {
			if _, ok := config.Vars[name]; ok || name == config.Binding.Name {
				return errors.New("BINDING_NAME_CONFLICT: A supplied secret conflicts with an ordinary var or Durable Object binding.")
			}
		}
	}
	a.warn(result.Warnings)

	namespace, err := a.Client.NamespaceSlug(ctx, noNamespace)
	if err != nil {
		return err
	}
	endpoint := api.AppPath(namespace, config.Name)
	err = a.Client.Do(ctx, http.MethodPost, "/namespaces/"+api.PathEscape(namespace)+"/apps",
		map[string]string{"name": config.Name, "access": config.Access}, nil,
		api.IdempotencyKey(api.NewIdempotencyKey()))
	if err != nil && !api.HasCode(err, "APP_NAME_TAKEN") {
		return err
	}
	current, err := a.Client.GetApp(ctx, namespace, config.Name)
	if err != nil {
		return err
	}
	if current.Access != config.Access {
		a.progress("Applying %s access from applet.jsonc (currently %s)...", config.Access, current.Access)
	}
	if err := a.Client.Do(ctx, http.MethodPatch, endpoint+"/access", map[string]string{"access": config.Access}, nil); err != nil {
		return err
	}
	artifact, err := result.Artifact(current.ID)
	if err != nil {
		return err
	}

	a.progress("Uploading artifact...")
	var created struct {
		Deployment api.Deployment `json:"deployment"`
		Upload     struct {
			URL    string `json:"url"`
			Method string `json:"method"`
		} `json:"upload"`
	}
	if err := a.Client.Do(ctx, http.MethodPost, endpoint+"/deployments", map[string]any{}, &created); err != nil {
		return err
	}
	if err := a.upload(ctx, created.Upload.Method, created.Upload.URL, artifact.Bytes); err != nil {
		return err
	}

	finalizeBody := map[string]any{"sha256": artifact.SHA256()}
	var finalizeOptions []api.RequestOption
	if supplied != nil {
		names := supplied.Names()
		a.progress("Publishing Worker with %d secrets: %s", len(names), strings.Join(names, ", "))
		finalizeBody["secrets"] = supplied.Secrets
		finalizeOptions = append(finalizeOptions, api.Timeout(90*time.Second))
	}
	deploymentPath := endpoint + "/deployments/" + api.PathEscape(created.Deployment.ID)
	var finalized struct {
		Deployment *api.Deployment `json:"deployment"`
	}
	if err := a.Client.Do(ctx, http.MethodPost, deploymentPath+"/finalize", finalizeBody, &finalized, finalizeOptions...); err != nil {
		if supplied == nil {
			return err
		}
		code := api.Code(err)
		if !slices.Contains([]string{"APP_BUSY", "BINDING_NAME_CONFLICT", "RATE_LIMITED", "INVALID_SECRET_VALUE", "INVALID_SECRET_NAME", "INVALID_DEPLOYMENT_STATE", "UNAUTHENTICATED", "FORBIDDEN", "ARTIFACT_HASH_MISMATCH"}, code) {
			code = "SECRET_OUTCOME_UNCERTAIN"
		}
		return fmt.Errorf("%s: Deployment with secrets did not complete normally. Inspect deployment status and the integration before retrying deliberately with the intended current credentials. Values are not queued or retried automatically.", code)
	}
	if finalized.Deployment != nil && finalized.Deployment.Status == "live" {
		return a.printDeployed(ctx, namespace, config.Name)
	}

	a.progress("Deployment queued: %s", created.Deployment.ID)
	for range 60 {
		if err := a.Sleep(ctx, 2*time.Second); err != nil {
			return err
		}
		var state struct {
			Deployment api.Deployment `json:"deployment"`
		}
		if err := a.Client.Do(ctx, http.MethodGet, deploymentPath, nil, &state); err != nil {
			return err
		}
		switch state.Deployment.Status {
		case "live":
			return a.printDeployed(ctx, namespace, config.Name)
		case "failed":
			if state.Deployment.ErrorMessage != nil {
				return errors.New(*state.Deployment.ErrorMessage)
			}
			return errors.New("Deployment failed.")
		}
	}
	a.println("Deployment is still processing. Check the dashboard for its status.")
	return nil
}

func (a *App) printDeployed(ctx context.Context, namespace, name string) error {
	current, err := a.Client.GetApp(ctx, namespace, name)
	if err != nil {
		return err
	}
	a.printf("Deployed: %s\n", current.URL)
	return nil
}

func (a *App) upload(ctx context.Context, method, url string, body []byte) error {
	if method == "" {
		method = http.MethodPut
	}
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("content-type", "application/gzip")
	response, err := a.HTTP.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Could not upload the deployment artifact (%v). Please retry; if this keeps happening, check your connection or contact Applet support.", err)
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("Could not upload the deployment artifact (HTTP %d). Please retry; if this keeps happening, check your connection or contact Applet support.", response.StatusCode)
	}
	return nil
}
