package adapters

// Agents' JSON config files, read and written keeping what the person wrote: keys in their order, numbers as
// written, and the layout the agent itself writes (JSON.stringify(v, null, 2) plus a newline), so a file the agent
// wrote comes back byte for byte apart from Teamflow's own entries.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/vutran1710/teamflow-cli/internal/home"
	"github.com/vutran1710/teamflow-cli/internal/ojson"
)

// ParseJSON reads a JSON object: objects as ojson.Obj (in order), arrays as []any, numbers as json.Number. Blank
// is an empty object. Anything malformed (or not an object, or with a key twice) is an error naming the file.
func ParseJSON(data []byte, name string) (ojson.Obj, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return ojson.Obj{}, nil
	}
	bad := func(why string) error {
		return fmt.Errorf("%s is not valid JSON (%s): fix it by hand, then run this again; nothing was changed", name, why)
	}
	if !utf8.Valid(data) {
		return nil, bad("not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, bad(err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, bad("data after the object")
	}
	obj, ok := v.(ojson.Obj)
	if !ok {
		return nil, bad("not an object")
	}
	return obj, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := ojson.Obj{}
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("a key that is not a string")
				}
				if seen[key] {
					return nil, fmt.Errorf("the key %q twice", key)
				}
				seen[key] = true
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj = append(obj, ojson.KV{Key: key, Value: v})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected %v", t)
	}
	return tok, nil
}

// EncodeJSON writes v as JSON.stringify(v, null, 2) does, plus a final newline.
func EncodeJSON(v any) []byte {
	var b bytes.Buffer
	encode(&b, v, "")
	b.WriteByte('\n')
	return b.Bytes()
}

func encode(b *bytes.Buffer, v any, indent string) {
	in := indent + "  "
	switch x := v.(type) {
	case ojson.Obj:
		if len(x) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, kv := range x {
			b.WriteString(in)
			encodeString(b, kv.Key)
			b.WriteString(": ")
			encode(b, kv.Value, in)
			if i < len(x)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range x {
			b.WriteString(in)
			encode(b, e, in)
			if i < len(x)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	case []string:
		arr := make([]any, len(x))
		for i, s := range x {
			arr[i] = s
		}
		encode(b, arr, indent)
	case string:
		encodeString(b, x)
	case json.Number:
		b.WriteString(x.String())
	case bool:
		fmt.Fprint(b, x)
	case nil:
		b.WriteString("null")
	case int:
		fmt.Fprint(b, x)
	default:
		raw, _ := ojson.Marshal(x)
		b.Write(raw)
	}
}

// encodeString escapes as JSON.stringify does: quote, backslash, and control characters only.
func encodeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// LoadJSON reads a JSON config file (missing: an empty object). A malformed one is refused, never overwritten.
func LoadJSON(path string) (ojson.Obj, error) {
	data, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseJSON(data, home.Display(path))
}

// SaveJSON writes a JSON config file (perm for a new one; an existing one keeps its mode).
func SaveJSON(path string, doc ojson.Obj, perm os.FileMode) error {
	return WriteFile(path, EncodeJSON(doc), perm)
}

// ------------------------------------------------------------------ editing an ordered document

// Obj is v as an object (false when it is something else).
func Obj(v any) (ojson.Obj, bool) {
	o, ok := v.(ojson.Obj)
	return o, ok
}

// Without is o without key (and whether it had it).
func Without(o ojson.Obj, key string) (ojson.Obj, bool) {
	for i, kv := range o {
		if kv.Key == key {
			return append(append(ojson.Obj{}, o[:i]...), o[i+1:]...), true
		}
	}
	return o, false
}

// Str is v as a string ("" when it is something else).
func Str(v any) string {
	s, _ := v.(string)
	return s
}
