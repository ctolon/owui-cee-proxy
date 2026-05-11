package httpclient

import (
	"net/http"

	"golang.org/x/time/rate"
)

// RateLimitedTransport wraps an inner http.RoundTripper and gates
// every outbound request through a per-engine token-bucket limiter.
// Closes C-24 from REVIEW-FAANG.md: the YAML `engines.<n>.rate_limit`
// knob was previously documented but never enforced. Operators
// throttling a flaky backend will now actually throttle it instead
// of relying on the global ratelimit_global.
//
// When the limiter rejects a token (Wait returns an error — usually
// because the request context fired its deadline while waiting), the
// transport surfaces the error to the adapter's do() rather than
// silently sending the request. The breaker logic ABOVE this layer
// keeps its own state, so a rate-limit-induced cancel does NOT count
// as a 5xx failure for breaker purposes.
type RateLimitedTransport struct {
	Limiter *rate.Limiter
	Next    http.RoundTripper
}

// RoundTrip blocks until the limiter admits the request OR the ctx
// fires. Errors propagated verbatim.
func (t *RateLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Limiter != nil {
		if err := t.Limiter.Wait(req.Context()); err != nil {
			return nil, err
		}
	}
	return t.Next.RoundTrip(req)
}
