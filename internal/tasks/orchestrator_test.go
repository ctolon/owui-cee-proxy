package tasks

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestMintToken_LengthAndUniqueness(t *testing.T) {
	t.Parallel()
	a, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	b, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}
	if a == b {
		t.Fatalf("two consecutive tokens collided: %s", a)
	}
}

func TestKeyFromAPIKey_Deterministic(t *testing.T) {
	t.Parallel()
	a := keyFromAPIKey("foo")
	b := keyFromAPIKey("foo")
	if a != b {
		t.Fatalf("non-deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("fingerprint is not hex: %v", err)
	}
	if c := keyFromAPIKey("bar"); c == a {
		t.Fatalf("expected different fingerprints for different keys")
	}
	if keyFromAPIKey("") != "" {
		t.Fatalf("empty input must yield empty fingerprint")
	}
}

func TestValidateBlobLimits(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name       string
		refs       []BlobRef
		maxBlob    int64
		maxTotal   int64
		wantErrSub string
	}{
		{
			name:    "ok within both caps",
			refs:    []BlobRef{{Size: 50}, {Size: 50}},
			maxBlob: 100, maxTotal: 200,
		},
		{
			name:       "single blob exceeds per-blob cap",
			refs:       []BlobRef{{Filename: "big.pdf", Size: 150}},
			maxBlob:    100,
			maxTotal:   1000,
			wantErrSub: "exceeds max_blob_bytes",
		},
		{
			name:       "aggregate exceeds total cap",
			refs:       []BlobRef{{Size: 60}, {Size: 60}},
			maxBlob:    100,
			maxTotal:   100,
			wantErrSub: "exceeds max_total_bytes",
		},
		{
			name:    "zero caps disable limits",
			refs:    []BlobRef{{Size: 1 << 30}},
			maxBlob: 0, maxTotal: 0,
		},
		{
			name:       "negative size rejected",
			refs:       []BlobRef{{Filename: "x", Size: -1}},
			maxBlob:    100,
			maxTotal:   100,
			wantErrSub: "size unknown",
		},
	}
	for _, tc := range tcs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &Payload{BlobKeys: tc.refs}
			err := validateBlobLimits(p, tc.maxBlob, tc.maxTotal)
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error %q missing substring %q", err.Error(), tc.wantErrSub)
			}
		})
	}
}
