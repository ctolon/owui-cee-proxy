package handlers

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSpool_BelowThresholdStaysInMemory(t *testing.T) {
	t.Parallel()
	const threshold = 64
	in := strings.NewReader("hello world")
	sr, err := spoolPart(in, threshold, 0)
	require.NoError(t, err)
	defer sr.Close()

	require.NotNil(t, sr.memBuf, "small payloads must stay in memory")
	require.Nil(t, sr.file)
	require.EqualValues(t, len("hello world"), sr.Size())

	out, err := io.ReadAll(sr)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(out))
}

func TestSpool_AboveThresholdSpoolsToDisk(t *testing.T) {
	t.Parallel()
	const threshold = 16
	payload := bytes.Repeat([]byte("A"), 4096)
	sr, err := spoolPart(bytes.NewReader(payload), threshold, 0)
	require.NoError(t, err)
	defer sr.Close()

	require.Nil(t, sr.memBuf, "large payloads must spill to disk")
	require.NotNil(t, sr.file)
	require.EqualValues(t, len(payload), sr.Size())

	out, err := io.ReadAll(sr)
	require.NoError(t, err)
	require.Equal(t, payload, out)
}

func TestSpool_CloseRemovesTempFileAndBlocksReads(t *testing.T) {
	t.Parallel()

	const threshold = 8
	sr, err := spoolPart(bytes.NewReader(bytes.Repeat([]byte("x"), 1024)), threshold, 0)
	require.NoError(t, err)

	path := sr.path // empty when O_TMPFILE was used (anonymous inode)
	require.NoError(t, sr.Close())
	if path != "" {
		_, statErr := os.Stat(path)
		require.True(t, os.IsNotExist(statErr),
			"temp file %q must be unlinked after Close (got err=%v)", path, statErr)
	}

	// Read after Close must error rather than return data.
	_, err = sr.Read(make([]byte, 4))
	require.Error(t, err)

	// Memory path: Close is idempotent and Read after Close still errors.
	sr2, err := spoolPart(strings.NewReader("hi"), 64, 0)
	require.NoError(t, err)
	require.NoError(t, sr2.Close())
	require.NoError(t, sr2.Close(), "Close must be idempotent")
	_, err = sr2.Read(make([]byte, 4))
	require.Error(t, err)
}

func TestSpool_RejectsOversize(t *testing.T) {
	t.Parallel()
	// Cap below threshold: the in-memory branch should reject.
	_, err := spoolPart(bytes.NewReader(bytes.Repeat([]byte("y"), 200)), 1024, 100)
	require.ErrorIs(t, err, ErrSpoolTooLarge)

	// Cap above threshold: the spill-to-disk branch should reject.
	_, err = spoolPart(bytes.NewReader(bytes.Repeat([]byte("z"), 4096)), 64, 1024)
	require.ErrorIs(t, err, ErrSpoolTooLarge)
}

func TestSpool_Seek(t *testing.T) {
	t.Parallel()
	sr, err := spoolPart(strings.NewReader("0123456789"), 64, 0)
	require.NoError(t, err)
	defer sr.Close()
	_, err = sr.Seek(5, io.SeekStart)
	require.NoError(t, err)
	out, err := io.ReadAll(sr)
	require.NoError(t, err)
	require.Equal(t, "56789", string(out))
}
