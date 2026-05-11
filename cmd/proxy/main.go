// Command proxy is the entrypoint for the OpenWebUI Content Extraction
// Engine proxy. It loads configuration, builds the application, and
// runs the HTTP server until SIGINT or SIGTERM is received.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ctolon/owui-cee-proxy/internal/app"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/version"
)

func main() {
	// Delegate to run so deferred cleanups (signal stop, etc.) actually
	// fire — calling os.Exit directly from main would skip them, which
	// is what the gocritic exitAfterDefer lint flags.
	os.Exit(run())
}

func run() int {
	var (
		cfgPath     string
		showVersion bool
		validate    bool
	)
	flag.StringVar(&cfgPath, "config", config.DefaultConfigPath(), "path to YAML config")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&validate, "validate", false, "load + validate the config and exit; no server starts. Exit codes: 0 on success, 2 on validation failure.")
	flag.Parse()

	if showVersion {
		v := version.Current()
		fmt.Printf("owui-cee-proxy %s (%s, %s)\n", v.Version, v.Commit, v.Date)
		return 0
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 2
	}

	if validate {
		// Operators wire this into a Helm pre-upgrade hook so a typo
		// in routing.strategy / mimedetect.extension_overrides / a
		// CR/LF in the resolved secret fails BEFORE the rolling
		// update touches a live pod. No server, no port binding;
		// just config.Load (already done above) + the implicit
		// config.Validate it runs internally. Exit 0 = config is
		// safe to roll; 2 means stop the rollout.
		fmt.Printf("owui-cee-proxy: config %q valid\n", cfgPath)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.Build(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n", err)
		return 2
	}

	if err := a.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		return 1
	}
	return 0
}
