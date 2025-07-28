package gorge

import "github.com/vmihailenco/msgpack/v5"

// MsgPackSerializer uses the vmihailenco/msgpack library for serialization.
// It is generally faster and produces smaller output than JSON.
type MsgPackSerializer struct{}

// Marshal implements the Serializer interface.
func (s MsgPackSerializer) Marshal(v interface{}) ([]byte, error) {
	return msgpack.Marshal(v)
}

// Unmarshal implements the Serializer interface.
func (s MsgPackSerializer) Unmarshal(data []byte, v interface{}) error {
	return msgpack.Unmarshal(data, v)
}
