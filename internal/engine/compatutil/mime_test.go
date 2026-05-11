package compatutil_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine/compatutil"
)

// TestSafeOutboundMIME_TableDriven pins the CRLF / control-byte
// defence. Inputs that look like a valid `type/subtype` survive
// unchanged; anything else (parameters, CR-LF injection attempts,
// empty strings, garbled tokens) collapses to
// application/octet-stream.
func TestSafeOutboundMIME_TableDriven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"pdf passes through", "application/pdf", "application/pdf"},
		{"vendor MIME passes", "application/vnd.ms-outlook", "application/vnd.ms-outlook"},
		{"plus subtype passes", "application/openxmlformats-officedocument.wordprocessingml.document", "application/openxmlformats-officedocument.wordprocessingml.document"},
		{"empty falls back", "", "application/octet-stream"},
		{"missing slash falls back", "applicationpdf", "application/octet-stream"},
		{"params rejected", "application/pdf; charset=binary", "application/octet-stream"},
		{"crlf injection rejected", "application/pdf\r\nX-Inject: 1", "application/octet-stream"},
		{"newline-only rejected", "application/pdf\n", "application/octet-stream"},
		{"space in token rejected", "application/p df", "application/octet-stream"},
		{"leading dot rejected", ".application/pdf", "application/octet-stream"},
		{"control byte rejected", "application/pdf\x00", "application/octet-stream"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, c.want, compatutil.SafeOutboundMIME(c.in))
		})
	}
}
