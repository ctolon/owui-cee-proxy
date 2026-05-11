package handlers

import (
	"io"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// fallbackEligible reports whether a primary engine's response
// warrants a single-step fallback retry. Transport-level errors
// (non-nil err) and HTTP 5xx responses are eligible; 4xx and 2xx
// are NOT — a 400 means the caller's request is malformed, retrying
// against another engine just produces the same 400; a 200 is
// success.
func fallbackEligible(resp *engine.ConvertResponse, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode >= 500
}

// statusOrZero returns resp.StatusCode or 0 when resp is nil. Used
// to populate the `primary_status` field on the
// `engine_fallback_attempt` log record without panicking on a
// transport-error path where resp == nil.
func statusOrZero(resp *engine.ConvertResponse) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// rewindFiles tries to Seek(0, SeekStart) every body on the request.
// Returns true iff every body implements io.Seeker AND the rewind
// succeeded. The handler skips the fallback when this returns false
// so we never half-send a partially-consumed body to the fallback
// engine.
func rewindFiles(req *engine.ConvertRequest) bool {
	for i := range req.Files {
		s, ok := req.Files[i].Body.(io.Seeker)
		if !ok {
			return false
		}
		if _, err := s.Seek(0, io.SeekStart); err != nil {
			return false
		}
	}
	return true
}
