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

// spoolInitialBuf is the bootstrap capacity for the in-memory buffer
// used by spoolPart. Sized large enough to absorb a typical option
// blob / short text payload in a single allocation, small enough
// that 100 concurrent uploads of <1 MiB files allocate ~6.4 MiB
// total at peak rather than the 800 MiB the old fixed pre-alloc
// produced. (C-16 from REVIEW-FAANG.md.)
const spoolInitialBuf = 64 << 10

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
//
// Memory growth: a bytes.Buffer grows geometrically from
// spoolInitialBuf up to threshold+1. Pre-fix this function allocated
// `make([]byte, threshold+1)` (= 8 MiB) regardless of actual file
// size — 800 MiB heap pressure at 100 concurrent small uploads
// (C-16 from REVIEW-FAANG.md). The buffer's amortised growth caps
// peak alloc at next_pow2(filesize) for sub-threshold files, which
// is what the budget actually targets.
func spoolPart(r io.Reader, threshold, maxBytes int64) (*spoolReader, error) {
	if threshold <= 0 {
		threshold = spoolThresholdDefault
	}
	// Start the buffer at a sensible bootstrap size so tiny files
	// don't thrash the first few growth steps. 64 KiB covers the
	// "small option blob" + "short text payload" tail of the
	// distribution at zero realistic cost.
	buf := bytes.NewBuffer(make([]byte, 0, spoolInitialBuf))
	// io.CopyN reads up to threshold+1 bytes. Returns:
	//   * (threshold+1, nil)   → overflow; spill needed
	//   * (n<threshold+1, EOF) → fit in memory; done
	//   * (n, other-err)       → real transport error
	n, err := io.CopyN(buf, r, threshold+1)
	switch {
	case errors.Is(err, io.EOF):
		if maxBytes > 0 && n > maxBytes {
			return nil, ErrSpoolTooLarge
		}
		return &spoolReader{
			memBuf: bytes.NewReader(buf.Bytes()),
			size:   n,
		}, nil
	case err != nil:
		return nil, err
	}
	// Overflow: n == threshold+1 and there is more left to read. Spill
	// the already-buffered bytes to a temp file then continue copying
	// from r.
	memBytes := buf.Bytes()
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
