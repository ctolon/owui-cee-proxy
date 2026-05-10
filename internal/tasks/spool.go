package tasks

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// spoolReader behaves like its sibling in package handlers: small parts
// stay in memory; large parts spill to a temp file that is unlinked on
// Close. We duplicate the type rather than depending on package
// handlers because tasks must remain free of transport coupling.
type spoolReader struct {
	memBuf *bytes.Reader
	file   *os.File
	path   string
	size   int64
	closed bool
}

func (s *spoolReader) Read(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("spool: read after close")
	}
	if s.memBuf != nil {
		return s.memBuf.Read(p)
	}
	return s.file.Read(p)
}

func (s *spoolReader) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.memBuf != nil {
		s.memBuf = nil
		return nil
	}
	if s.file == nil {
		return nil
	}
	closeErr := s.file.Close()
	if s.path != "" {
		if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && closeErr == nil {
			closeErr = rmErr
		}
	}
	s.file = nil
	return closeErr
}

func (s *spoolReader) Size() int64 { return s.size }

// errSpoolTooLarge signals that the source exceeded the caller-supplied
// per-part cap. Surfaced to the handler as a 4xx (M-class blob limit
// error) via the orchestrator's isClientError helper.
var errSpoolTooLarge = errors.New("spool: part exceeds maximum size")

// spoolPart reads at most maxBytes from r (0 disables the cap),
// keeping bytes in memory until threshold is exceeded.
func spoolPart(r io.Reader, threshold, maxBytes int64) (*spoolReader, error) {
	if threshold <= 0 {
		threshold = 8 << 20
	}
	mem := make([]byte, threshold+1)
	n, err := io.ReadFull(r, mem)
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		if maxBytes > 0 && int64(n) > maxBytes {
			return nil, errSpoolTooLarge
		}
		return &spoolReader{memBuf: bytes.NewReader(mem[:n]), size: int64(n)}, nil
	case err != nil:
		return nil, err
	}
	memBytes := mem[:int64(n)]
	f, err := os.CreateTemp("", "owui-cee-spool-*")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	if _, err := f.Write(memBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	total := int64(n)
	var rest io.Reader = r
	if maxBytes > 0 {
		rest = &io.LimitedReader{R: r, N: maxBytes - total + 1}
	}
	more, copyErr := io.Copy(f, rest)
	total += more
	if copyErr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, copyErr
	}
	if maxBytes > 0 && total > maxBytes {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, errSpoolTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &spoolReader{file: f, path: path, size: total}, nil
}
