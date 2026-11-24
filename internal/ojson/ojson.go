// Package ojson is a JSON object that keeps its keys in the order they were added, as a Python dict does: the MCP
// tools answer with the Python tools' objects, field for field and in their order.
package ojson

import (
	"bytes"
	"encoding/json"
)

// KV is one field. An alias of a plain struct, so fields are written {"key", value}.
type KV = struct {
	Key   string
	Value any
}

// Obj is an ordered JSON object.
type Obj []KV

// Get is the value of a key (nil, false when absent).
func (o Obj) Get(key string) (any, bool) {
	for _, kv := range o {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return nil, false
}

// Set replaces a key's value, or adds the key at the end.
func (o Obj) Set(key string, v any) Obj {
	for i, kv := range o {
		if kv.Key == key {
			o[i].Value = v
			return o
		}
	}
	return append(o, KV{Key: key, Value: v})
}

// MarshalJSON writes the fields in order. HTML characters are kept as they are, as Python writes them.
func (o Obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := Marshal(kv.Key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		v, err := Marshal(kv.Value)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Marshal is json.Marshal without HTML escaping.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// Indent is v as JSON indented by two spaces, as pydantic's to_json(indent=2) writes it.
func Indent(v any) (string, error) {
	raw, err := Marshal(v)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return "", err
	}
	return b.String(), nil
}
