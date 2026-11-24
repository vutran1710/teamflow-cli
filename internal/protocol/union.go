package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

var errNotObject = errors.New("not a JSON object")

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// unionFields reads a union value's top-level fields; it must be an object.
func unionFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return nil, errNotObject
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errNotObject
	}
	return fields, nil
}

// unionTag reads the field naming a union's variant: "" when it is missing, an error when it is not a string.
func unionTag(raw json.RawMessage, prop string) (string, error) {
	fields, err := unionFields(raw)
	if err != nil {
		return "", err
	}
	t, ok := fields[prop]
	if !ok {
		return "", nil
	}
	var tag string
	if err := json.Unmarshal(t, &tag); err != nil {
		return "", fmt.Errorf("%s is not a string", prop)
	}
	return tag, nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }

func rawOrNull(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	return raw
}

// decodeOne decodes a union field; missing or null is the zero value (nil).
func decodeOne[T any](raw json.RawMessage, decode func(json.RawMessage) (T, error)) (T, error) {
	var zero T
	if len(raw) == 0 || isNull(raw) {
		return zero, nil
	}
	return decode(raw)
}

// decodeList decodes an array of union values; missing or null is nil.
func decodeList[T any](raw json.RawMessage, decode func(json.RawMessage) (T, error)) ([]T, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf(": %w", err)
	}
	out := make([]T, len(items))
	for i, item := range items {
		v, err := decode(item)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// shape is one variant of a union whose tag may be left out: its required fields and all its fields.
type shape struct {
	tag              string
	required, fields []string
	closed           bool // the variant allows no other field
}

func (s shape) fits(present map[string]json.RawMessage) bool {
	for _, r := range s.required {
		if _, ok := present[r]; !ok {
			return false
		}
	}
	if !s.closed {
		return true
	}
	for k := range present {
		known := false
		for _, f := range s.fields {
			if f == k {
				known = true
				break
			}
		}
		if !known {
			return false
		}
	}
	return true
}

// pickByShape gives the tag of the one variant raw fits, or "" when it fits none or several.
func pickByShape(raw json.RawMessage, shapes []shape) string {
	present, err := unionFields(raw)
	if err != nil {
		return ""
	}
	pick, n := "", 0
	for _, s := range shapes {
		if s.fits(present) {
			pick, n = s.tag, n+1
		}
	}
	if n != 1 {
		return ""
	}
	return pick
}
