// Package build bundles an Applet project with esbuild and packages it as an
// .applet.tgz v1 deployment artifact.
package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/applet-one/cli/internal/project"
)

var entryPattern = regexp.MustCompile(`(?i)\.(?:jsx?|tsx?|mjs|mts)$`)

// Options configure a build.
type Options struct {
	Dir string
	// ForbiddenFile is a resolved path that Worker code must not import, such
	// as a secrets file supplied to the same deployment.
	ForbiddenFile string
}

// Result is a bundled Worker.
type Result struct {
	Config   *Config
	Source   []byte
	Warnings []string
}

// Build validates the project in opts.Dir and bundles its Worker entry point.
func Build(ctx context.Context, opts Options) (*Result, error) {
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	p, err := project.Load(dir)
	if err != nil {
		return nil, err
	}
	config, err := Validate(p)
	if err != nil {
		return nil, err
	}
	main := filepath.Join(dir, config.Main)
	rel, err := filepath.Rel(dir, main)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(config.Main) {
		return nil, errors.New("'main' must point to a file inside the project.")
	}
	if !entryPattern.MatchString(main) {
		return nil, errors.New("Applet v1 Worker entry points must be JavaScript or TypeScript modules.")
	}
	if ctx.Err() != nil {
		return nil, errors.New("Build cancelled.")
	}

	var plugins []api.Plugin
	if opts.ForbiddenFile != "" {
		plugins = append(plugins, forbidFile(opts.ForbiddenFile))
	}
	buildContext, contextErr := api.Context(api.BuildOptions{
		AbsWorkingDir: dir,
		EntryPoints:   []string{main},
		Outfile:       filepath.Join(dir, "worker.js"),
		Bundle:        true,
		Write:         false,
		Format:        api.FormatESModule,
		Platform:      api.PlatformBrowser,
		Conditions:    []string{"workerd", "worker", "browser", "module", "import", "default"},
		Target:        api.ES2022,
		Splitting:     false,
		Metafile:      true,
		Sourcemap:     api.SourceMapNone,
		LogLevel:      api.LogLevelSilent,
		Plugins:       plugins,
	})
	if contextErr != nil {
		return nil, buildError(dir, contextErr.Errors)
	}
	defer buildContext.Dispose()
	stop := context.AfterFunc(ctx, buildContext.Cancel)
	result := buildContext.Rebuild()
	stop()
	if ctx.Err() != nil {
		return nil, errors.New("Build cancelled.")
	}
	if len(result.Errors) > 0 {
		return nil, buildError(dir, result.Errors)
	}

	var source []byte
	for _, file := range result.OutputFiles {
		if strings.HasSuffix(file.Path, ".js") {
			source = file.Contents
			break
		}
	}
	if source == nil {
		return nil, errors.New("Builder did not emit a Worker module.")
	}
	var metafile struct {
		Outputs map[string]struct {
			EntryPoint string   `json:"entryPoint"`
			Exports    []string `json:"exports"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(result.Metafile), &metafile); err != nil {
		return nil, fmt.Errorf("Could not read the build metadata: %w", err)
	}
	var exports []string
	for _, output := range metafile.Outputs {
		if output.EntryPoint != "" {
			exports = output.Exports
			break
		}
	}
	if !slices.Contains(exports, config.Binding.ClassName) {
		return nil, fmt.Errorf("Worker must export the configured Durable Object class '%s' from its entry point (%s).", config.Binding.ClassName, config.Main)
	}
	if !slices.Contains(exports, "default") {
		return nil, fmt.Errorf("Worker must have a default export from its entry point (%s).", config.Main)
	}
	return &Result{Config: config, Source: source, Warnings: p.Warnings()}, nil
}

func forbidFile(forbidden string) api.Plugin {
	return api.Plugin{
		Name: "exclude-secrets-file",
		Setup: func(build api.PluginBuild) {
			build.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: "file"}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				// Missing files are left for esbuild to report.
				if resolved, err := filepath.EvalSymlinks(args.Path); err == nil && resolved == forbidden {
					return api.OnLoadResult{Errors: []api.Message{{Text: "Secrets files must not be imported by Worker code."}}}, nil
				}
				return api.OnLoadResult{}, nil
			})
		},
	}
}

func buildError(dir string, messages []api.Message) error {
	var details []string
	for _, message := range messages {
		if message.Location != nil {
			// esbuild reports paths relative to the working directory.
			file := message.Location.File
			if rel, err := filepath.Rel(dir, file); err == nil && filepath.IsAbs(file) {
				file = rel
			}
			details = append(details, fmt.Sprintf("  %s:%d:%d: %s", file, message.Location.Line, message.Location.Column+1, message.Text))
		} else {
			details = append(details, "  "+message.Text)
		}
	}
	return fmt.Errorf("Could not build your Worker:\n%s", strings.Join(details, "\n"))
}
