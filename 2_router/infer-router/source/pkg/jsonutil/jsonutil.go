package jsonutil

import (
	"io"

	gojson "github.com/goccy/go-json"
)

// Marshal serializes v to JSON bytes.
// This is the unified JSON facade using go-json for reduced allocations.
func Marshal(v any) ([]byte, error) {
	return gojson.Marshal(v)
}

// Unmarshal deserializes JSON data into v.
func Unmarshal(data []byte, v any) error {
	return gojson.Unmarshal(data, v)
}

// NewEncoder creates a JSON encoder that writes to w.
func NewEncoder(w io.Writer) *gojson.Encoder {
	return gojson.NewEncoder(w)
}

// NewDecoder creates a JSON decoder that reads from r.
func NewDecoder(r io.Reader) *gojson.Decoder {
	return gojson.NewDecoder(r)
}
