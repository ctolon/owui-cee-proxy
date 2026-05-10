package config

import (
	"encoding/json"

	"github.com/knadh/koanf/v2"
)

// structsLoader returns a koanf provider that serializes a struct using JSON
// tags then parses it back as raw map. Used to seed defaults.
type structsProvider struct {
	v any
}

func structsLoader(v any) koanf.Provider { return &structsProvider{v: v} }

func (s *structsProvider) ReadBytes() ([]byte, error) {
	return nil, errReadNotSupported
}

func (s *structsProvider) Read() (map[string]any, error) {
	b, err := json.Marshal(s.v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return remapKeys(m), nil
}

// remapKeys converts JSON struct field names to lowercase to align with
// koanf YAML/env keys (since yaml/env keys are lowercase).
func remapKeys(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		k = lowerFirst(k)
		switch t := v.(type) {
		case map[string]any:
			out[k] = remapKeys(t)
		default:
			out[k] = v
		}
	}
	return out
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}

type configErr string

func (c configErr) Error() string { return string(c) }

const errReadNotSupported configErr = "ReadBytes not supported"
