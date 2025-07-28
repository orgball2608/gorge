package gorge

import "encoding/json"

// Serializer defines the interface for converting data between struct and []byte.
type Serializer interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

// JSONSerializer is the default implementation using JSON.
type JSONSerializer struct{}

func (s JSONSerializer) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (s JSONSerializer) Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
