// Package dev runs an Applet project locally on workerd, the open-source
// Cloudflare Workers runtime.
package dev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/applet-one/cli/internal/build"
	"github.com/applet-one/cli/internal/dotenv"
)

// Options configure a local development server.
type Options struct {
	Dir    string
	Host   string
	Port   int
	Stdout io.Writer
	Stderr io.Writer
	// Ready is called once the server accepts connections.
	Ready func(url string, warnings []string)
}

// Run builds the project and serves it until ctx is cancelled. Durable
// Object state persists under .applet/dev.
func Run(ctx context.Context, opts Options) error {
	result, err := build.Build(ctx, build.Options{Dir: opts.Dir})
	if err != nil {
		return err
	}
	if err := CheckAddress(opts.Host, opts.Port); err != nil {
		return err
	}
	devVars, err := LoadDevVars(opts.Dir, []string{result.Config.Binding.Name})
	if err != nil {
		return err
	}
	workerd, err := WorkerdPath(ctx, opts.Stderr)
	if err != nil {
		return err
	}
	stateDir, err := filepath.Abs(filepath.Join(opts.Dir, ".applet", "dev"))
	if err != nil {
		return err
	}
	configPath, err := writeRuntimeConfig(stateDir, result, devVars, opts.Host, opts.Port)
	if err != nil {
		return err
	}

	command := exec.Command(workerd, "serve", configPath)
	command.Stdout = opts.Stdout
	stderr := &startupWriter{out: opts.Stderr}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("Could not start the local Workers runtime: %w", err)
	}
	// exited is closed once the runtime exits; waitErr is then its status.
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(exited)
	}()
	exitErr := func() error {
		if waitErr == nil {
			return errors.New("exited before accepting connections")
		}
		return waitErr
	}

	if err := waitReady(ctx, opts.Host, opts.Port, exited, exitErr); err != nil {
		stop(command, exited)
		if ctx.Err() != nil {
			return errors.New("Development startup cancelled.")
		}
		return startupError(err, stderr.String())
	}
	stderr.passthrough()
	if opts.Ready != nil {
		opts.Ready("http://"+net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)), result.Warnings)
	}
	select {
	case <-ctx.Done():
		stop(command, exited)
		return nil
	case <-exited:
		return fmt.Errorf("The local Workers runtime stopped unexpectedly: %w", exitErr())
	}
}

func stop(command *exec.Cmd, exited <-chan struct{}) {
	if runtime.GOOS == "windows" {
		_ = command.Process.Kill()
	} else {
		_ = command.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		<-exited
	}
}

func waitReady(ctx context.Context, host string, port int, exited <-chan struct{}, exitErr func() error) error {
	address := net.JoinHostPort(dialHost(host), strconv.Itoa(port))
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			return exitErr()
		case <-deadline:
			return errors.New("timed out waiting for the runtime to accept connections")
		case <-ticker.C:
			if conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
				conn.Close()
				// workerd listens before evaluating the Worker's top-level code;
				// give a failing script a moment to exit.
				select {
				case <-exited:
					return exitErr()
				case <-time.After(500 * time.Millisecond):
					return nil
				}
			}
		}
	}
}

func dialHost(host string) string {
	switch host {
	case "0.0.0.0", "":
		return "127.0.0.1"
	case "::", "[::]":
		return "::1"
	}
	return host
}

func startupError(cause error, stderr string) error {
	var details []string
	for line := range strings.SplitSeq(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "stack:") {
			details = append(details, line)
		}
		if len(details) == 2 {
			break
		}
	}
	message := "The local Workers runtime could not start. Check your Worker code and Applet configuration."
	if len(details) > 0 {
		return fmt.Errorf("%s Details: %s", message, strings.Join(details, " "))
	}
	return fmt.Errorf("%s (%v)", message, cause)
}

// startupWriter buffers runtime stderr until startup succeeds, so startup
// failures can be summarized, then passes output through.
type startupWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	out    io.Writer
	live   bool
}

func (w *startupWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.live {
		return w.out.Write(p)
	}
	return w.buffer.Write(p)
}

func (w *startupWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func (w *startupWriter) passthrough() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.live = true
	_, _ = w.out.Write(w.buffer.Bytes())
	w.buffer.Reset()
}

// CheckAddress verifies host:port can be listened on.
func CheckAddress(host string, port int) error {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err == nil {
		return listener.Close()
	}
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, syscall.EADDRINUSE) || strings.Contains(message, "address already in use") || strings.Contains(message, "only one usage of each socket address"):
		next := 8788
		if port < 65535 {
			next = port + 1
		}
		return fmt.Errorf("Port %d is already in use. Stop the other process or choose another port with --port (for example, --port %d).", port, next)
	case errors.Is(err, syscall.EADDRNOTAVAIL) || strings.Contains(message, "can't assign requested address") || strings.Contains(message, "cannot assign requested address") || strings.Contains(message, "no such host"):
		return fmt.Errorf("Cannot listen on %s. Use an IP address assigned to this computer, or omit --host to use 127.0.0.1.", host)
	}
	return fmt.Errorf("Cannot check whether %s:%d is available: %v", host, port, err)
}

var devVarPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// LoadDevVars reads the git-ignored .dev.vars file used only by local dev.
func LoadDevVars(dir string, bindingNames []string) (map[string]string, error) {
	path := filepath.Join(dir, ".dev.vars")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Size() > 32*1024 {
		return nil, errors.New("Local .dev.vars exceeds 32 KiB.")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := dotenv.Parse(string(data))
	for name := range values {
		if !devVarPattern.MatchString(name) || slices.Contains(bindingNames, name) {
			return nil, errors.New("Local .dev.vars contains an invalid name or Durable Object binding collision.")
		}
	}
	return values, nil
}

// writeRuntimeConfig writes the bundled module and a workerd configuration
// under stateDir/runtime and returns the configuration path.
func writeRuntimeConfig(stateDir string, result *build.Result, devVars map[string]string, host string, port int) (string, error) {
	runtimeDir := filepath.Join(stateDir, "runtime")
	storageDir := filepath.Join(stateDir, "durable-objects")
	for _, dir := range []string{runtimeDir, storageDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "worker.mjs"), result.Source, 0o644); err != nil {
		return "", err
	}
	c := result.Config
	vars := map[string]string{}
	maps.Copy(vars, c.Vars)
	maps.Copy(vars, devVars)

	var bindings []string
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		bindings = append(bindings, fmt.Sprintf("(name = %s, text = %s)", capnpString(name), capnpString(vars[name])))
	}
	bindings = append(bindings, fmt.Sprintf("(name = %s, durableObjectNamespace = (className = %s))", capnpString(c.Binding.Name), capnpString(c.Binding.ClassName)))
	var flags []string
	for _, flag := range c.CompatibilityFlags {
		flags = append(flags, capnpString(flag))
	}

	config := fmt.Sprintf(`using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    (name = %[1]s, worker = .worker),
    (name = "durable-object-storage", disk = (path = %[2]s, writable = true)),
  ],
  sockets = [(name = "http", address = %[3]s, http = (), service = %[1]s)],
);

const worker :Workerd.Worker = (
  modules = [(name = "worker.mjs", esModule = embed "worker.mjs")],
  compatibilityDate = %[4]s,
  compatibilityFlags = [%[5]s],
  bindings = [
    %[6]s,
  ],
  durableObjectNamespaces = [
    (className = %[7]s, uniqueKey = %[8]s, enableSql = true),
  ],
  durableObjectStorage = (localDisk = "durable-object-storage"),
);
`,
		capnpString(c.Name),
		capnpString(storageDir),
		capnpString(net.JoinHostPort(host, strconv.Itoa(port))),
		capnpString(c.CompatibilityDate),
		strings.Join(flags, ", "),
		strings.Join(bindings, ",\n    "),
		capnpString(c.Binding.ClassName),
		capnpString(c.Name+"-"+c.Binding.ClassName),
	)
	path := filepath.Join(runtimeDir, "config.capnp")
	return path, os.WriteFile(path, []byte(config), 0o644)
}

// capnpString quotes s as a Cap'n Proto text literal.
func capnpString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
