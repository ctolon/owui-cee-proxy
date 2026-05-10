// Command migrate-config translates a v0.1.x owui-cee-proxy config
// into the v0.2.0 schema. Reads YAML from stdin or a path argument
// and emits YAML to stdout.
//
// Translations applied:
//
//	routing.default_cee_engine        → routing.default_engine
//	routing.facade_path_prefix        → routing.facade.docling.prefix
//	routing.passthrough.docling_prefix → (dropped — passthrough is now per-engine /<name>/*)
//	routing.passthrough.tika_prefix    → (same)
//	routing.passthrough.kreuzberg_prefix → (same)
//
//	engines.docling                   → engines.docling   { compat_type: docling, auth_header: X-Api-Key }
//	engines.tika                      → engines.tika      { compat_type: tika,    auth_header: X-Api-Key }
//	engines.kreuzberg                 → engines.kreuzberg { compat_type: docling, auth_header: Authorization, auth_scheme: bearer }
//
// Run: `go run scripts/migrate-config <old.yaml> > new.yaml`
package main

import (
	"fmt"
	"io"
	"maps"
	"os"

	"gopkg.in/yaml.v3"
)

type oldConfig struct {
	Server  map[string]any `yaml:"server,omitempty"`
	Routing struct {
		DefaultCEEEngine string `yaml:"default_cee_engine"`
		FacadePathPrefix string `yaml:"facade_path_prefix"`
		Passthrough      struct {
			DoclingPrefix   string `yaml:"docling_prefix"`
			TikaPrefix      string `yaml:"tika_prefix"`
			KreuzbergPrefix string `yaml:"kreuzberg_prefix"`
		} `yaml:"passthrough,omitempty"`
	} `yaml:"routing"`
	Engines struct {
		Docling   map[string]any `yaml:"docling,omitempty"`
		Tika      map[string]any `yaml:"tika,omitempty"`
		Kreuzberg map[string]any `yaml:"kreuzberg,omitempty"`
	} `yaml:"engines"`
	Security        map[string]any `yaml:"security,omitempty"`
	RatelimitGlobal map[string]any `yaml:"ratelimit_global,omitempty"`
	Tasks           map[string]any `yaml:"tasks,omitempty"`
	Observability   map[string]any `yaml:"observability,omitempty"`
}

func main() {
	var (
		raw []byte
		err error
	)
	if len(os.Args) > 1 {
		// #nosec G304 G703 -- migrate-config is a developer CLI; the
		// caller deliberately points it at a YAML file on disk.
		raw, err = os.ReadFile(os.Args[1])
	} else {
		raw, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "read input: %v\n", err)
		os.Exit(2)
	}

	var old oldConfig
	if err := yaml.Unmarshal(raw, &old); err != nil {
		fmt.Fprintf(os.Stderr, "parse yaml: %v\n", err)
		os.Exit(2)
	}

	out := map[string]any{}
	if old.Server != nil {
		out["server"] = old.Server
	}

	routing := map[string]any{
		"default_engine": old.Routing.DefaultCEEEngine,
		"facade": map[string]any{
			"docling": map[string]any{
				"enabled": true,
				"prefix":  pickStringDefault(old.Routing.FacadePathPrefix, "/v1"),
			},
			"external": map[string]any{
				"enabled": false,
				"path":    "/process",
			},
		},
		"passthrough": map[string]any{
			"enabled": true,
		},
	}
	out["routing"] = routing

	engines := map[string]any{}
	if old.Engines.Docling != nil {
		engines["docling"] = annotateEngine(old.Engines.Docling, "docling", "X-Api-Key", "raw")
	}
	if old.Engines.Tika != nil {
		engines["tika"] = annotateEngine(old.Engines.Tika, "tika", "X-Api-Key", "raw")
	}
	if old.Engines.Kreuzberg != nil {
		engines["kreuzberg"] = annotateEngine(old.Engines.Kreuzberg, "docling", "Authorization", "bearer")
	}
	out["engines"] = engines

	if old.Security != nil {
		out["security"] = old.Security
	}
	if old.RatelimitGlobal != nil {
		out["ratelimit_global"] = old.RatelimitGlobal
	}
	if old.Tasks != nil {
		out["tasks"] = old.Tasks
	}
	if old.Observability != nil {
		out["observability"] = old.Observability
	}

	enc := yaml.NewEncoder(os.Stdout)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "encode yaml: %v\n", err)
		os.Exit(2)
	}
	_ = enc.Close()
}

// annotateEngine returns a copy of the legacy engine block with the
// new compat_type, auth_header, and auth_scheme keys layered in.
// Existing keys (enable, url, api_key_env, request_timeout, etc.) are
// preserved as-is.
func annotateEngine(in map[string]any, compatType, authHeader, authScheme string) map[string]any {
	out := map[string]any{}
	maps.Copy(out, in)
	out["compat_type"] = compatType
	out["auth_header"] = authHeader
	out["auth_scheme"] = authScheme
	return out
}

func pickStringDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
