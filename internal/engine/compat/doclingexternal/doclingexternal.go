// Package doclingexternal implements the composite Engine adapter for
// backends that expose BOTH /v1/convert/file (docling-shape) AND
// /process (external-shape) at separate paths on the same URL. The
// proxy routes each request to whichever child matches the inbound
// facade.
package doclingexternal

import (
	"context"
	"errors"
	"fmt"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/docling"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/external"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

type Adapter struct {
	name engine.Name
	doc  *docling.Adapter
	ext  *external.Adapter
}

func New(name engine.Name, cfg config.EngineConfig, client *httpclient.Client, br *breaker.Breaker) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("docling-external: url is required")
	}
	d, err := docling.New(name, cfg, client, br)
	if err != nil {
		return nil, fmt.Errorf("docling-external: docling child: %w", err)
	}
	e, err := external.New(name, cfg, client, br)
	if err != nil {
		return nil, fmt.Errorf("docling-external: external child: %w", err)
	}
	return &Adapter{name: name, doc: d, ext: e}, nil
}

func (a *Adapter) Name() engine.Name { return a.name }

func (a *Adapter) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{
		Facades:     []engine.Facade{engine.FacadeDocling, engine.FacadeExternal},
		HTTPSources: true, // delegated to docling child when source mode is requested
	}
}

func (a *Adapter) Convert(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	switch req.Facade {
	case engine.FacadeDocling:
		return a.doc.Convert(ctx, req)
	case engine.FacadeExternal:
		return a.ext.Convert(ctx, req)
	default:
		return nil, fmt.Errorf("docling-external: unknown facade %q", req.Facade)
	}
}

// Health probes both children. The composite is healthy only when both
// endpoint paths are reachable — operators may want to override with
// a single dedicated health_path that covers both.
func (a *Adapter) Health(ctx context.Context) error {
	if err := a.doc.Health(ctx); err != nil {
		return fmt.Errorf("docling child: %w", err)
	}
	if err := a.ext.Health(ctx); err != nil {
		return fmt.Errorf("external child: %w", err)
	}
	return nil
}
