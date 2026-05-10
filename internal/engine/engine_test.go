package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeEngine struct {
	name Name
}

func (f *fakeEngine) Name() Name { return f.name }
func (f *fakeEngine) Convert(_ context.Context, _ *ConvertRequest) (*ConvertResponse, error) {
	return &ConvertResponse{StatusCode: 200}, nil
}
func (f *fakeEngine) Health(_ context.Context) error { return nil }

func TestNewRegistry_DefaultMustExist(t *testing.T) {
	t.Parallel()
	_, err := NewRegistry(map[Name]RegistryEntry{
		Docling: {Engine: &fakeEngine{name: Docling}},
	}, Tika)
	require.ErrorIs(t, err, ErrUnknownEngine)
}

func TestNewRegistry_GetAndDefault(t *testing.T) {
	t.Parallel()
	d := &fakeEngine{name: Docling}
	tk := &fakeEngine{name: Tika}
	r, err := NewRegistry(map[Name]RegistryEntry{
		Docling: {Engine: d},
		Tika:    {Engine: tk},
	}, Docling)
	require.NoError(t, err)
	require.Equal(t, d, r.Default())

	got, err := r.Get(Tika)
	require.NoError(t, err)
	require.Equal(t, tk, got)

	_, err = r.Get("missing")
	require.True(t, errors.Is(err, ErrUnknownEngine))

	require.ElementsMatch(t, []Name{Docling, Tika}, r.Names())
}

func TestNewRegistry_Empty(t *testing.T) {
	t.Parallel()
	_, err := NewRegistry(nil, Docling)
	require.Error(t, err)
}

func TestRegistry_PickByMime(t *testing.T) {
	t.Parallel()
	d := &fakeEngine{name: Docling}
	tk := &fakeEngine{name: Tika}
	kb := &fakeEngine{name: Kreuzberg}

	r, err := NewRegistry(map[Name]RegistryEntry{
		Docling:   {Engine: d},
		Tika:      {Engine: tk, MimeTypes: []string{"application/pdf", "image/*"}},
		Kreuzberg: {Engine: kb, MimeTypes: []string{"application/vnd.openxmlformats-officedocument.wordprocessingml.document"}},
	}, Docling)
	require.NoError(t, err)

	cases := []struct {
		mime string
		want Engine
	}{
		{"application/pdf", tk},
		{"application/pdf; charset=utf-8", tk},
		{"APPLICATION/PDF", tk},
		{"image/png", tk},
		{"image/jpeg", tk},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", kb},
		{"application/octet-stream", d},
		{"", d},
		{"text/plain", d},
	}
	for _, c := range cases {
		got := r.Pick(c.mime)
		require.Same(t, c.want, got, "mime=%q", c.mime)
	}
}

func TestRegistry_PickIgnoresDefaultMimeRules(t *testing.T) {
	t.Parallel()
	// Even if default engine declares mime types, they are ignored:
	// non-default engines are checked first; default is the catch-all.
	d := &fakeEngine{name: Docling}
	tk := &fakeEngine{name: Tika}

	r, err := NewRegistry(map[Name]RegistryEntry{
		Docling: {Engine: d, MimeTypes: []string{"application/pdf"}},
		Tika:    {Engine: tk, MimeTypes: []string{"application/pdf"}},
	}, Docling)
	require.NoError(t, err)

	require.Same(t, tk, r.Pick("application/pdf"), "non-default tika should win")
}
