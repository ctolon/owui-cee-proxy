package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeEngine struct {
	name    Name
	caps    EngineCapabilities
	convert func(*ConvertRequest) (*ConvertResponse, error)
}

func (f *fakeEngine) Name() Name                       { return f.name }
func (f *fakeEngine) URL() string                      { return "http://" + string(f.name) + ".test" }
func (f *fakeEngine) Capabilities() EngineCapabilities { return f.caps }
func (f *fakeEngine) Convert(_ context.Context, r *ConvertRequest) (*ConvertResponse, error) {
	if f.convert != nil {
		return f.convert(r)
	}
	return &ConvertResponse{StatusCode: 200}, nil
}
func (f *fakeEngine) Health(_ context.Context) error { return nil }

// docFacade returns capabilities with only the docling facade.
func docFacade() EngineCapabilities {
	return EngineCapabilities{Facades: []Facade{FacadeDocling}}
}

func bothFacades() EngineCapabilities {
	return EngineCapabilities{Facades: []Facade{FacadeDocling, FacadeExternal}}
}

func TestNewRegistry_DefaultMustExist(t *testing.T) {
	t.Parallel()
	_, err := NewRegistry(map[Name]RegistryEntry{
		"a": {Engine: &fakeEngine{name: "a", caps: docFacade()}},
	}, "missing", "")
	require.ErrorIs(t, err, ErrUnknownEngine)
}

func TestNewRegistry_GetAndDefault(t *testing.T) {
	t.Parallel()
	d := &fakeEngine{name: "primary", caps: docFacade()}
	tk := &fakeEngine{name: "secondary", caps: docFacade()}
	r, err := NewRegistry(map[Name]RegistryEntry{
		"primary":   {Engine: d},
		"secondary": {Engine: tk},
	}, "primary", "")
	require.NoError(t, err)
	require.Equal(t, d, r.Default())

	got, err := r.Get("secondary")
	require.NoError(t, err)
	require.Equal(t, tk, got)

	_, err = r.Get("missing")
	require.True(t, errors.Is(err, ErrUnknownEngine))

	require.ElementsMatch(t, []Name{"primary", "secondary"}, r.Names())
}

func TestRegistry_PickByMimeAndFacade(t *testing.T) {
	t.Parallel()
	d := &fakeEngine{name: "default-eng", caps: bothFacades()}
	pdf := &fakeEngine{name: "pdf-eng", caps: docFacade()}
	img := &fakeEngine{name: "img-eng", caps: bothFacades()}

	r, err := NewRegistry(map[Name]RegistryEntry{
		"default-eng": {Engine: d},
		"pdf-eng":     {Engine: pdf, MimeTypes: []string{"application/pdf"}},
		"img-eng":     {Engine: img, MimeTypes: []string{"image/*"}},
	}, "default-eng", "")
	require.NoError(t, err)

	cases := []struct {
		facade Facade
		mime   string
		want   Engine
	}{
		{FacadeDocling, "application/pdf", pdf},
		{FacadeDocling, "image/png", img},
		{FacadeDocling, "text/plain", d},
		{FacadeExternal, "application/pdf", d}, // pdf-eng doesn't accept external; fall through
		{FacadeExternal, "image/png", img},     // img-eng accepts external
		{FacadeExternal, "text/plain", d},
	}
	for _, c := range cases {
		got := r.Pick(c.facade, c.mime)
		require.Same(t, c.want, got, "facade=%s mime=%s", c.facade, c.mime)
	}
}

func TestRegistry_PickWithError_NoEngineForFacade(t *testing.T) {
	t.Parallel()
	d := &fakeEngine{name: "doc-only", caps: docFacade()}
	r, err := NewRegistry(map[Name]RegistryEntry{
		"doc-only": {Engine: d},
	}, "doc-only", "")
	require.NoError(t, err)

	_, err = r.PickWithError(FacadeExternal, "application/pdf")
	require.ErrorIs(t, err, ErrNoEngineForFacade)

	got, err := r.PickWithError(FacadeDocling, "application/pdf")
	require.NoError(t, err)
	require.Same(t, d, got)
}

func TestEngineCapabilities_AcceptsFacade(t *testing.T) {
	t.Parallel()
	c := bothFacades()
	require.True(t, c.AcceptsFacade(FacadeDocling))
	require.True(t, c.AcceptsFacade(FacadeExternal))
	require.False(t, c.AcceptsFacade("unknown"))
}
