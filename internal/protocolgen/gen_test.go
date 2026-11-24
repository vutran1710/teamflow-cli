package protocolgen

import (
	"strings"
	"testing"
)

const okMethods = `{"account.me": {"params": "Empty", "result": "Empty", "who": "account"}}`

func schemaWith(defs, constants string) string {
	return `{"$defs": {` + defs + `}, "x-constants": {` + constants + `}}`
}

func TestASmallSchemaGenerates(t *testing.T) {
	files, err := Generate([]byte(schemaWith(`"Empty": {"type": "object", "properties": {}}`, `"MAX_BATCH": 50`)), []byte(okMethods))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files["types_gen.go"]), "type Empty struct") ||
		!strings.Contains(string(files["constants_gen.go"]), "MaxBatch = 50") {
		t.Errorf("unexpected output:\n%s\n%s", files["types_gen.go"], files["constants_gen.go"])
	}
	if !strings.HasPrefix(string(files["types_gen.go"]), "// Code generated") {
		t.Error("generated files must say so")
	}
}

func TestTheGeneratorRefusesWhatItCannotMap(t *testing.T) {
	empty := `"Empty": {"type": "object", "properties": {}}`
	cases := map[string]struct{ schema, methods, want string }{
		"not JSON":           {`{nope`, okMethods, "schema.json"},
		"not an object":      {`[1]`, okMethods, "not an object"},
		"no $defs":           {`{"x-constants": {"A": 1}}`, okMethods, "no $defs"},
		"no constants":       {`{"$defs": {` + empty + `}}`, okMethods, "no x-constants"},
		"an unknown $ref":    {schemaWith(`"A": {"type": "object", "properties": {"b": {"$ref": "#/$defs/Nope"}}}`, `"A": 1`), okMethods, "unknown $def"},
		"an unsupported def": {schemaWith(`"A": {"type": "integer"}`, `"A": 1`), okMethods, "unsupported"},
		"an unsupported field": {schemaWith(`"A": {"type": "object", "properties": {"b": {"type": "tuple"}}}`, `"A": 1`),
			okMethods, "unsupported schema"},
		"an inline object": {schemaWith(`"A": {"type": "object", "properties": {"b": {"type": "object", "properties": {"c": {"type": "string"}}}}}`, `"A": 1`),
			okMethods, "inline object"},
		"a union branch that is not a $ref": {schemaWith(`"A": {"oneOf": [{"type": "string"}, {"type": "integer"}]}`, `"A": 1`),
			okMethods, "not a $ref"},
		"variants with nothing to tell them apart": {schemaWith(empty+`, "B": {"type": "object", "properties": {}}, "U": {"oneOf": [{"$ref": "#/$defs/Empty"}, {"$ref": "#/$defs/B"}]}`, `"A": 1`),
			okMethods, "no required field of its own"},
		"an enum value that is not a string": {schemaWith(`"E": {"type": "string", "enum": [1]}`, `"A": 1`), okMethods, "not a string"},
		"colliding fields": {schemaWith(`"A": {"type": "object", "properties": {"a_b": {"type": "string"}, "a.b": {"type": "string"}}}`, `"A": 1`),
			okMethods, "collide"},
		"an unsupported default": {schemaWith(`"A": {"type": "object", "properties": {"b": {"type": "array", "items": {"type": "string"}, "default": ["x"]}}}`, `"A": 1`),
			okMethods, "unsupported default"},
		"a constant that is a list": {schemaWith(empty, `"A": [1]`), okMethods, "unsupported constant"},
		"methods not JSON":          {schemaWith(empty, `"A": 1`), `{nope`, "methods.json"},
		"a method without who":      {schemaWith(empty, `"A": 1`), `{"m": {"params": "Empty", "result": "Empty"}}`, "has no who"},
		"no methods":                {schemaWith(empty, `"A": 1`), `{}`, "no methods"},
	}
	for name, c := range cases {
		_, err := Generate([]byte(c.schema), []byte(c.methods))
		if err == nil {
			t.Errorf("%s: generated without an error", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, c.want)
		}
	}
}

func TestGoNames(t *testing.T) {
	for in, want := range map[string]string{
		"id": "ID", "client_session": "ClientSession", "UPGRADE_REQUIRED": "UpgradeRequired",
		"signal.accepted": "SignalAccepted", "key_points.v1": "KeyPointsV1", "RPC_PATH": "RPCPath",
		"content_b64": "ContentB64", "sha256": "Sha256", "1x": "X1x",
	} {
		if got := goName(in); got != want {
			t.Errorf("goName(%q) = %q, want %q", in, got, want)
		}
	}
}
