package handlers

import (
	"net/http"
	"net/http/httputil"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// Passthrough binds a *httputil.ReverseProxy to a specific engine name
// so the access logger and metrics can label requests correctly even
// when no body parsing happens (the proxy streams raw bytes).
type Passthrough struct {
	Engine engine.Name
	Proxy  *httputil.ReverseProxy
	URL    string
}

func (p *Passthrough) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := mw.WithEngine(r.Context(), string(p.Engine), p.URL)
	p.Proxy.ServeHTTP(w, r.WithContext(ctx))
}
