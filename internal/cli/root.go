// Package cli implements the applet command-line interface.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/applet-one/cli/internal/api"
	"github.com/applet-one/cli/internal/auth"
	"github.com/applet-one/cli/internal/update"
	"github.com/applet-one/cli/internal/version"
)

const docsURL = "https://applet.one"

// App holds the dependencies shared by commands.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Dir is the project directory.
	Dir    string
	Client *api.Client
	Store  *auth.Store
	// HTTP is used for requests outside the Applet API (artifact uploads,
	// backup downloads).
	HTTP *http.Client
	// Sleep waits between polls.
	Sleep func(ctx context.Context, d time.Duration) error
	// IsTerminal reports whether stdin (and stdout) are interactive.
	IsTerminal func() bool
	// OpenBrowser opens a URL, best effort.
	OpenBrowser func(url string)

	stdinReader *bufio.Reader
}

// NewApp returns an App wired to the process environment.
func NewApp() (*App, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	path, err := auth.DefaultPath()
	if err != nil {
		return nil, err
	}
	store := &auth.Store{Path: path}
	base := os.Getenv("APPLET_API_URL")
	if base == "" {
		base = api.DefaultBaseURL
	}
	return &App{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Dir:    dir,
		Store:  store,
		Client: &api.Client{
			BaseURL:   strings.TrimRight(base, "/"),
			HTTP:      &http.Client{Timeout: 2 * time.Minute},
			Token:     store.Token,
			UserAgent: "applet-cli/" + version.Version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")",
		},
		HTTP: &http.Client{Timeout: 5 * time.Minute},
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		},
		IsTerminal: func() bool {
			return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
		},
		OpenBrowser: openBrowser,
	}, nil
}

func openBrowser(url string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if command.Start() == nil {
		go func() { _ = command.Wait() }()
	}
}

func (a *App) printf(format string, args ...any) { fmt.Fprintf(a.Stdout, format, args...) }

func (a *App) println(args ...any) { fmt.Fprintln(a.Stdout, args...) }

func (a *App) progress(format string, args ...any) { fmt.Fprintf(a.Stderr, format+"\n", args...) }

func (a *App) warn(warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(a.Stderr, "Warning: %s\n", warning)
	}
}

// readLine prompts on stderr and reads one line from stdin.
func (a *App) readLine(prompt string) (string, error) {
	fmt.Fprint(a.Stderr, prompt)
	if a.stdinReader == nil {
		a.stdinReader = bufio.NewReader(a.Stdin)
	}
	line, err := a.stdinReader.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context, app *App, args []string) int {
	// `pnpm run <script> -- <args>` forwards the separator.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	} else if len(args) > 1 && args[1] == "--" {
		args = append([]string{args[0]}, args[2:]...)
	}

	var notice func() string
	if os.Getenv("APPLET_NO_UPDATE_CHECK") == "" && term.IsTerminal(int(os.Stderr.Fd())) {
		notice = update.Check(ctx, version.Version, time.Now().Add(time.Second))
	}

	root := NewRootCommand(app)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)

	if notice != nil {
		if latest := notice(); latest != "" {
			fmt.Fprintf(app.Stderr, "\nNew version available: %s (current: %s)\nUpdate: %s\n\n", latest, version.Version, update.ReleasesURL)
		}
	}
	if err == nil {
		return 0
	}
	var exit exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	if errors.Is(err, context.Canceled) {
		err = errors.New("Cancelled.")
	}
	fmt.Fprintf(app.Stderr, "applet: %s\n", err)
	return 1
}

// exitError exits with a code after the command has printed its own output.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// NewRootCommand builds the command tree.
func NewRootCommand(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "applet",
		Short: "Create, deploy, and manage Cloudflare Worker apps",
		Long:  "Applet creates, deploys, and manages Cloudflare Worker apps.\n\nDocumentation: " + docsURL,
		Example: `  applet init my-app
  applet init .        Scaffold the current directory
  applet deploy`,
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetIn(app.Stdin)
	root.SetOut(app.Stdout)
	root.SetErr(app.Stderr)
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return fmt.Errorf("%v\nUsage: %s", err, strings.TrimSuffix(cmd.UseLine(), " [flags]"))
	})
	root.CompletionOptions.HiddenDefaultCmd = true

	root.AddCommand(
		newInitCommand(app),
		newMigrateConfigCommand(app),
		newVersionCommand(app),
		newLoginCommand(app),
		newLogoutCommand(app),
		newStatusCommand(app),
		newDevCommand(app),
		newBuildCommand(app),
		newDeployCommand(app),
		newBackupCommand(app),
		newAccessCommand(app),
		newURLCommand(app),
		newLogsCommand(app),
		newSecretsCommand(app),
		newDeleteCommand(app),
	)
	return root
}

// usageArgs validates positional arguments, failing with the command's usage.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return fmt.Errorf("Usage: %s", strings.TrimSuffix(cmd.UseLine(), " [flags]"))
		}
		return nil
	}
}

func newVersionCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the CLI version",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			app.println(version.Version)
			return nil
		},
	}
}
