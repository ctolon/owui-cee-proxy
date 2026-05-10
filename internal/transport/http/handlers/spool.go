package handlers

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// spoolThresholdDefault is the in-memory cap before a part is spilled
// to a temp file. 8 MiB matches the H1 mitigation in docs/REVIEW.md.
const spoolThresholdDefault = 8 << 20

// spoolReader is the union return type of spoolPart. It implements
// io.ReadSeekCloser so adapters that want to retry / restart can rewind
// (the docling streaming writer does not, but other adapters might).
//
// Either memBuf is non-nil (small parts), or file is non-nil (large
// parts spilled to disk). Close() on a spilled reader removes the
// underlying temp file; Read after Close errors out.
type spoolReader struct {
	memBuf *bytes.Reader
	file   *os.File
	// path captures the on-disk location for unlink-after-close. Empty
	// when O_TMPFILE was used (the file is already unlinked) or when the
	// part stayed in memory.
	path   string
	size   int64
	closed bool
}

// Read implements io.Reader.
func (s *spoolReader) Read(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("spool: read after close")
	}
	if s.memBuf != nil {
		return s.memBuf.Read(p)
	}
	return s.file.Read(p)
}

// Seek implements io.Seeker so retry-capable callers can rewind.
func (s *spoolReader) Seek(offset int64, whence int) (int64, error) {
	if s.closed {
		return 0, errors.New("spool: seek after close")
	}
	if s.memBuf != nil {
		return s.memBuf.Seek(offset, whence)
	}
	return s.file.Seek(offset, whence)
}

// Close releases the in-memory buffer or unlinks the temp file. Safe to
// call multiple times.
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

// Size returns the byte count actually read into the spool. Useful for
// FileBlob.Size so adapters that emit Content-Length can populate it.
func (s *spoolReader) Size() int64 { return s.size }

// spoolPart reads from r up to maxBytes (>0; 0 disables the cap). Below
// threshold the bytes stay in memory; above threshold we spill to a
// temp file (O_TMPFILE on Linux when available, falling back to
// os.CreateTemp). Returns ErrSpoolTooLarge if the source exceeds
// maxBytes; the partial spool is cleaned up before returning.
func spoolPart(r io.Reader, threshold, maxBytes int64) (*spoolReader, error) {
	if threshold <= 0 {
		threshold = spoolThresholdDefault
	}
	// Read up to threshold+1 to detect overflow without growing the
	// in-memory buffer.
	mem := make([]byte, threshold+1)
	n, err := io.ReadFull(r, mem)
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// Whole part fit (n bytes).
		if maxBytes > 0 && int64(n) > maxBytes {
			return nil, ErrSpoolTooLarge
		}
		return &spoolReader{
			memBuf: bytes.NewReader(mem[:n]),
			size:   int64(n),
		}, nil
	case err != nil:
		return nil, err
	}
	// Overflow: n == len(mem) and there is more left to read. Spill the
	// already-read bytes to a temp file then continue copying from r.
	memBytes := mem[:int64(n)]
	f, path, ferr := openSpoolFile()
	if ferr != nil {
		return nil, ferr
	}
	written, copyErr := f.Write(memBytes)
	if copyErr != nil {
		_ = f.Close()
		if path != "" {
			_ = os.Remove(path)
		}
		return nil, copyErr
	}
	total := int64(written)
	// Cap the rest of the copy at maxBytes (if set) so a malicious
	// streaming source cannot fill the disk past the configured limit.
	rest := r
	if maxBytes > 0 {
		// +1 so we can detect overflow.
		rest = &io.LimitedReader{R: r, N: maxBytes - total + 1}
	}
	more, copyErr := io.Copy(f, rest)
	total += more
	if copyErr != nil {
		_ = f.Close()
		if path != "" {
			_ = os.Remove(path)
		}
		return nil, copyErr
	}
	if maxBytes > 0 && total > maxBytes {
		_ = f.Close()
		if path != "" {
			_ = os.Remove(path)
		}
		return nil, ErrSpoolTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		if path != "" {
			_ = os.Remove(path)
		}
		return nil, err
	}
	return &spoolReader{file: f, path: path, size: total}, nil
}

// ErrSpoolTooLarge is returned by spoolPart when the source stream
// exceeds the caller-supplied byte cap.
var ErrSpoolTooLarge = errors.New("spool: part exceeds maximum size")
