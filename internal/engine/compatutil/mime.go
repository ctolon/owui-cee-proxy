package compatutil

import "regexp"

// outboundMIMERE pins the structural shape of a MIME we forward to an
// engine backend. Same grammar as config.mimeValueRE (we keep two
// copies rather than introducing a layering edge from `engine/compat`
// → `config`): `type/subtype`, RFC 6838 token alphabet, no parameters,
// no whitespace, length-capped.
//
// Rejecting parameter strings is intentional. Each adapter strips
// params on its own (mimedetect.Resolve drops everything after `;`),
// so any survivor here is either a hand-rolled forward by a future
// adapter author OR a client-injected CR/LF/control-byte payload
// dressed as a parameter. Fail closed.
var outboundMIMERE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}$`)

// SafeOutboundMIME returns declared unchanged when it parses as a
// clean `type/subtype` MIME, or "application/octet-stream" when it
// does not. Adapter call sites that stamp a Content-Type from a
// caller-controlled FileBlob field MUST flow the value through this
// helper before invoking `http.Header.Set` — that is the defense
// against the CR/LF / control-byte header-injection class (CWE-93).
//
// Empty input also returns the octet-stream fallback so adapters
// never set an empty Content-Type on the wire.
func SafeOutboundMIME(declared string) string {
	if !outboundMIMERE.MatchString(declared) {
		return "application/octet-stream"
	}
	return declared
}
