package handlers

import (
	"fmt"
	"io"

	"github.com/ctolon/owui-cee-proxy/internal/engine/mimedetect"
)

// mimeSniffHeadBytes is the number of leading bytes we peek out of a
// spool reader before passing it to the engine adapter. 512 covers
// every magic-byte signature mimetype.Detect recognises (the longest
// signature in the package is 264 bytes); peeking more wastes bytes
// the adapter will re-read anyway, less risks a short-buffer false
// negative on rare formats.
const mimeSniffHeadBytes = 512

// peekAndResolveMIME inspects the first mimeSniffHeadBytes of sr,
// runs mimedetect.Resolve against the declared Content-Type, and
// rewinds sr to the start so the adapter's later io.Copy still sees
// the full body byte-for-byte.
//
// sr MUST be an io.ReadSeeker (every *spoolReader is). If sniffing
// fails on the rewind step (filesystem trouble for the disk-spilled
// case), the error is propagated to the caller so a bad spool
// surfaces as a 4xx instead of a silent body-truncation downstream.
func peekAndResolveMIME(declared string, sr io.ReadSeeker) (mimedetect.Result, error) {
	head := make([]byte, mimeSniffHeadBytes)
	n, err := io.ReadFull(sr, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return mimedetect.Result{}, fmt.Errorf("peek head: %w", err)
	}
	if _, err := sr.Seek(0, io.SeekStart); err != nil {
		return mimedetect.Result{}, fmt.Errorf("rewind: %w", err)
	}
	return mimedetect.Resolve(declared, head[:n]), nil
}
