package resources

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"slices"

	"github.com/grafana/grafana-app-sdk/logging"
	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/grafana/grafanapi/internal/logs"
	"github.com/grafana/grafanapi/internal/resources"
	"github.com/grafana/grafanapi/internal/resources/local"
	"github.com/grafana/grafanapi/internal/server"
	"github.com/grafana/grafanapi/internal/server/livereload"
	"github.com/grafana/grafanapi/internal/server/watch"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type serveOpts struct {
	Address       string
	Port          int
	WatchPaths    []string
	NoWatch       bool
	Script        string
	ScriptFormat  string
	MaxConcurrent int
}

func (opts *serveOpts) setup(flags *pflag.FlagSet) {
	flags.StringVar(&opts.Address, "address", "127.0.0.1", "Address to bind (loopback by default; any other address exposes the server, which acts with your Grafana session, to the network)")
	flags.IntVar(&opts.Port, "port", 8080, "Port on which the server will listen")
	flags.StringArrayVarP(&opts.WatchPaths, "watch", "w", nil, "Paths to watch for changes")
	flags.BoolVar(&opts.NoWatch, "no-watch", opts.NoWatch, "Do not watch for changes")
	flags.StringVarP(&opts.Script, "script", "S", "", "Script to execute to generate a resource")
	flags.StringVarP(&opts.ScriptFormat, "script-format", "f", "json", "Format of the data returned by the script")
	flags.IntVar(&opts.MaxConcurrent, "max-concurrent", 10, "Maximum number of concurrent operations")
}

func (opts *serveOpts) watchTargets(args []string) []string {
	if opts.NoWatch {
		return nil
	}

	return slices.Concat(args, opts.WatchPaths)
}

func (opts *serveOpts) Validate() error {
	if opts.MaxConcurrent < 1 {
		return errors.New("max-concurrent must be greater than zero")
	}

	return nil
}

func serveCmd(configOpts *cmdconfig.Options) *cobra.Command {
	opts := &serveOpts{}

	cmd := &cobra.Command{
		Use:   "serve [RESOURCE_DIR]...",
		Args:  cobra.ArbitraryArgs,
		Short: "Serve Grafana resources locally",
		Long: `Serve Grafana resources locally.

The server started by this command makes it easy to explore and review resources
locally.

While resources are loaded from disk, the server will use the Grafana instance
described in the current context to access some data (example: to run queries
when previewing dashboards).

Security: the server listens on 127.0.0.1 by default and prints a URL carrying
a per-run access token. Open that URL once: the token is exchanged for a
session cookie and changes on every run. Requests with a foreign Host header
are rejected (421), requests without the token (401) or that a browser marks
as coming from another origin or site (403) too. A non-loopback --address
prints a warning: the server acts with your Grafana session and becomes
reachable from other machines.

Note on NFS/SMB and watch mode: fsnotify requires support from underlying
OS to work. The current NFS and SMB protocols does not provide network level
support for file notifications.
`,
		Example: `
	# Serve resources from a directory:
	grafanapi resources serve ./resources

	# Serve resources from a directory but don't watch for changes:
	grafanapi resources serve ./resources --no-watch

	# Expose the server to the network (it acts with your Grafana session, so
	# anyone who obtains the token can use that session):
	grafanapi resources serve ./resources --address 0.0.0.0

	# Serve resources from a script that outputs a YAML resource and watch for changes:
	# Note: the Grafana Foundation SDK can be used to generate dashboards (https://grafana.github.io/grafana-foundation-sdk/)
	grafanapi resources serve --script 'go run dashboard-generator/*.go' --watch ./dashboard-generator --script-format yaml
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.Validate(); err != nil {
				return err
			}

			cfg, err := configOpts.LoadConfig(cmd.Context())
			if err != nil {
				return err
			}

			logger := logging.FromContext(cmd.Context())
			parsedResources := resources.NewResources()
			reader := local.FSReader{
				Decoders:           format.Codecs(),
				StopOnError:        false,
				MaxConcurrentReads: opts.MaxConcurrent,
			}

			if len(args) != 0 {
				if err := reader.Read(cmd.Context(), parsedResources, resources.Filters{}, args); err != nil {
					return err
				}
			}

			parseFromScript := func() error {
				output, err := executeWatchScript(cmd.Context(), opts.Script)
				if err != nil {
					return err
				}

				if err = reader.ReadBytes(cmd.Context(), parsedResources, output, opts.ScriptFormat); err != nil {
					logger.Warn("Could not parse script output", logs.Err(err))
					return err
				}

				return nil
			}

			if opts.Script != "" {
				if err := parseFromScript(); err != nil {
					return err
				}
			}

			//nolint:nestif
			if len(opts.watchTargets(args)) > 0 {
				livereload.Initialize()

				parsedResources.OnChange(func(resource *resources.Resource) {
					logger.Debug("Resource changed in memory", slog.String("resource", string(resource.Ref())))
					livereload.ReloadResource(resource)
				})

				// By default, react to changes by parsing changed files
				onInputChange := func(file string) {
					object := &resources.Resource{}
					if err = reader.ReadFile(cmd.Context(), object, file); err != nil {
						logger.Warn("Could not parse file", slog.String("file", file), logs.Err(err))
						return
					}

					parsedResources.Add(object)
				}

				// If a script is given, run the script on change
				if opts.Script != "" {
					onInputChange = func(_ string) {
						if err := parseFromScript(); err != nil {
							_, _ = cmd.ErrOrStderr().Write([]byte(err.Error()))
						}
					}
				}

				watcher, err := watch.NewWatcher(cmd.Context(), onInputChange)
				if err != nil {
					return err
				}

				if err := watcher.Add(opts.watchTargets(args)...); err != nil {
					return err
				}

				// Start listening for events.
				go watcher.Watch()
			}

			serverCfg := server.Config{
				ListenAddr: opts.Address,
				Port:       opts.Port,
				NoColor:    cmd.Flags().Lookup("no-color").Value.String() == "true",
			}
			resourceServer := server.New(serverCfg, cfg.GetCurrentContext(), parsedResources)

			ln, err := resourceServer.Listen(cmd.Context())
			if err != nil {
				return err
			}

			reportListening(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts.Address, resourceServer.URL())

			return resourceServer.Serve(cmd.Context(), ln)
		},
	}

	opts.setup(cmd.Flags())

	return cmd
}

func executeWatchScript(ctx context.Context, command string) ([]byte, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	logger := logging.FromContext(ctx).With(slog.String("component", "script"), slog.String("command", command))
	logger.Debug("executing script")

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}

	// If the script exits with a non-zero code, stderr will be used to populate an error.
	// Otherwise, we ensure that the output will at least be logged.
	defer func() {
		if stderr.Len() > 0 {
			logger.Warn("script stderr", slog.String("output", stderr.String()))
		}
	}()

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		details := stderr.String()
		stderr.Reset()

		return nil, fail.DetailedError{
			Summary: "Script failed",
			Details: details,
			Parent:  err,
		}
	}

	return stdout.Bytes(), nil
}

func reportListening(stdout io.Writer, stderr io.Writer, address string, url string) {
	cmdio.Info(stdout, "Server is listening on %s", url)

	if !server.IsLoopbackAddress(address) {
		cmdio.Warning(stderr, "--address %s is not a loopback address: the server acts with your Grafana session and is reachable from other machines; keep the URL's token secret", address)
	}
}
