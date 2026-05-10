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
	)
	flag.StringVar(&cfgPath, "config", envOr("OWUI_PROXY_CONFIG", "configs/config.example.yaml"), "path to YAML config")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
