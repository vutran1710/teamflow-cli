package protocol

// Contract tests: the Go types against the protocol exported from the server (../../protocol).
//
// A golden message is decoded into its Go type, encoded again, and compared with the original field by field.
// The comparison knows the schema: a field one side leaves out matches only when the other side holds that
// field's schema default (pydantic treats a missing field and its default as the same value), and numbers
// compare by value. Everything else must be equal.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/vutran1710/teamflow-cli/internal/protocolgen"
)

const exportDir = "../../protocol"

// ---------------------------------------------------------------- loading

type schemaDoc struct {
	Defs      map[string]any `json:"$defs"`
	Constants map[string]any `json:"x-constants"`
}

func readJSON(t *testing.T, path string) any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return v
}

func loadSchema(t *testing.T) schemaDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(exportDir, "schema.json"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var s schemaDoc
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	if len(s.Defs) == 0 {
		t.Fatal("schema has no $defs")
	}
	return s
}

type methodEntry struct {
	Params string `json:"params"`
	Result string `json:"result"`
	Who    string `json:"who"`
}

func loadMethods(t *testing.T) map[string]methodEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(exportDir, "methods.json"))
	if err != nil {
		t.Fatalf("read methods: %v", err)
	}
	var m map[string]methodEntry
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse methods: %v", err)
	}
	return m
}

func goldenFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(exportDir, "golden", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden files (%v)", err)
	}
	sort.Strings(files)
	return files
}

// piece is one message inside a golden file and the $def it is an instance of.
type piece struct {
	where string
	def   string
	raw   any
}

// pieces lists what a golden file holds. Envelope pieces carry def "" and are checked as JSON-RPC envelopes.
func pieces(t *testing.T, file string, methods map[string]methodEntry) []piece {
	t.Helper()
	g, ok := readJSON(t, file).(map[string]any)
	if !ok {
		t.Fatalf("%s: not an object", file)
	}
	name := filepath.Base(file)
	var out []piece
	switch g["kind"] {
	case "rpc":
		method, _ := g["method"].(string)
		m, ok := methods[method]
		if !ok {
			t.Fatalf("%s: method %q is not in methods.json", name, method)
		}
		if p, ok := g["params"]; ok {
			out = append(out, piece{name + " params", m.Params, p})
		}
		resp, _ := g["response"].(map[string]any)
		out = append(out, piece{name + " response", "", resp})
		if r, ok := resp["result"]; ok {
			out = append(out, piece{name + " result", m.Result, r})
		}
		if e, ok := resp["error"]; ok {
			out = append(out, piece{name + " error", "RpcError", e})
		}
	case "event":
		out = append(out, piece{name, "SyncEvent", g["event"]})
	case "live":
		msg, _ := g["message"].(map[string]any)
		def := "LiveMessage"
		if msg["type"] == "hello" {
			def = "LiveHello"
		}
		out = append(out, piece{name, def, msg})
	case "envelope":
		calls := map[string]string{}
		if reqs, ok := g["request"].([]any); ok {
			for i, r := range reqs {
				out = append(out, piece{fmt.Sprintf("%s request[%d]", name, i), "", r})
				if call, ok := r.(map[string]any); ok {
					if method, ok := call["method"].(string); ok {
						calls[fmt.Sprint(call["id"])] = method
					}
				}
			}
		}
		if req, ok := g["request"].(map[string]any); ok {
			out = append(out, piece{name + " request", "", req})
		}
		answers, isBatch := g["response"].([]any)
		if !isBatch {
			answers = []any{g["response"]}
		}
		for i, a := range answers {
			out = append(out, piece{fmt.Sprintf("%s response[%d]", name, i), "", a})
			ans, _ := a.(map[string]any)
			if r, ok := ans["result"]; ok {
				if m, ok := methods[calls[fmt.Sprint(ans["id"])]]; ok {
					out = append(out, piece{fmt.Sprintf("%s result[%d]", name, i), m.Result, r})
				}
			}
			if e, ok := ans["error"]; ok {
				out = append(out, piece{fmt.Sprintf("%s error[%d]", name, i), "RpcError", e})
			}
		}
	default:
		t.Fatalf("%s: unknown golden kind %v", name, g["kind"])
	}
	return out
}

// ---------------------------------------------------------------- decode and encode through the Go types

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// roundTrip decodes raw as the Go type of def and encodes it again. def "" is a JSON-RPC envelope: a Response
// when it carries a result or an error, otherwise a Request.
func roundTrip(def string, raw []byte) ([]byte, any, error) {
	var v any
	switch def {
	case "":
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, nil, err
		}
		_, isResult := probe["result"]
		_, isError := probe["error"]
		if isResult || isError {
			v = new(Response)
		} else {
			v = new(Request)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			return nil, nil, err
		}
	case "SyncEvent":
		e, err := DecodeSyncEvent(raw)
		if err != nil {
			return nil, nil, err
		}
		v = e
	case "LiveMessage":
		m, err := DecodeLiveMessage(raw)
		if err != nil {
			return nil, nil, err
		}
		v = m
	default:
		v = newDef(def)
		if v == nil {
			return nil, nil, fmt.Errorf("no Go type for $def %s", def)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			return nil, nil, err
		}
	}
	out, err := json.Marshal(v)
	return out, v, err
}

func parse(b []byte) any {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		panic(err)
	}
	return v
}

// ---------------------------------------------------------------- schema-aware semantic comparison

type comparer struct{ defs map[string]any }

func (c comparer) resolve(node any) map[string]any {
	n, _ := node.(map[string]any)
	for n != nil {
		ref, ok := n["$ref"].(string)
		if !ok {
			return n
		}
		n, _ = c.defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
	}
	return map[string]any{}
}

func (c comparer) nodeFor(def string) map[string]any {
	if def == "" {
		return map[string]any{}
	}
	return c.resolve(map[string]any{"$ref": "#/$defs/" + def})
}

// diff lists every difference between want (the golden) and got (what Go encoded), as paths.
func (c comparer) diff(path string, node map[string]any, want, got any) []string {
	for _, key := range []string{"oneOf", "anyOf"} {
		if branches, ok := node[key].([]any); ok {
			var best []string
			for i, b := range branches {
				d := c.diff(path, c.resolve(b), want, got)
				if len(d) == 0 {
					return nil
				}
				if i == 0 || len(d) < len(best) {
					best = d
				}
			}
			return best
		}
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want an object, got %s", path, mustJSON(got))}
		}
		return c.diffObject(path, node, w, g)
	case []any:
		g, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want an array, got %s", path, mustJSON(got))}
		}
		if len(w) != len(g) {
			return []string{fmt.Sprintf("%s: want %d items, got %d", path, len(w), len(g))}
		}
		items := c.resolve(node["items"])
		var out []string
		for i := range w {
			out = append(out, c.diff(fmt.Sprintf("%s[%d]", path, i), items, w[i], g[i])...)
		}
		return out
	case json.Number:
		if g, ok := got.(json.Number); ok && sameNumber(w, g) {
			return nil
		}
		return []string{fmt.Sprintf("%s: want %s, got %s", path, w, mustJSON(got))}
	default:
		if reflect.DeepEqual(want, got) {
			return nil
		}
		return []string{fmt.Sprintf("%s: want %s, got %s", path, mustJSON(want), mustJSON(got))}
	}
}

func sameNumber(a, b json.Number) bool {
	if a == b {
		return true
	}
	x, err1 := strconv.ParseFloat(string(a), 64)
	y, err2 := strconv.ParseFloat(string(b), 64)
	return err1 == nil && err2 == nil && x == y
}

func (c comparer) diffObject(path string, node, want, got map[string]any) []string {
	props, _ := node["properties"].(map[string]any)
	required := map[string]bool{}
	if req, ok := node["required"].([]any); ok {
		for _, r := range req {
			required[r.(string)] = true
		}
	}
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []string
	for _, k := range sorted {
		w, inWant := want[k]
		g, inGot := got[k]
		sub := c.fieldNode(node, k)
		switch {
		case inWant && inGot:
			out = append(out, c.diff(path+"."+k, sub, w, g)...)
		case inWant:
			if !c.isDefault(props, required, k, w) {
				out = append(out, fmt.Sprintf("%s.%s: lost (golden has %s)", path, k, mustJSON(w)))
			}
		default:
			if !c.isDefault(props, required, k, g) {
				out = append(out, fmt.Sprintf("%s.%s: added %s (not in the golden)", path, k, mustJSON(g)))
			}
		}
	}
	return out
}

func (c comparer) fieldNode(node map[string]any, key string) map[string]any {
	if props, ok := node["properties"].(map[string]any); ok {
		if p, ok := props[key]; ok {
			return c.resolve(p)
		}
	}
	if pats, ok := node["patternProperties"].(map[string]any); ok {
		for pat, p := range pats {
			if regexp.MustCompile(pat).MatchString(key) {
				return c.resolve(p)
			}
		}
	}
	if addl, ok := node["additionalProperties"].(map[string]any); ok {
		return c.resolve(addl)
	}
	return map[string]any{}
}

// isDefault: a field present on one side only is the same value when it is optional and holds its default.
func (c comparer) isDefault(props map[string]any, required map[string]bool, key string, v any) bool {
	if required[key] {
		return false
	}
	p, ok := props[key].(map[string]any)
	if !ok {
		return false
	}
	def, ok := p["default"]
	if !ok {
		return false
	}
	return len(c.diff("", map[string]any{}, def, v)) == 0
}

// ---------------------------------------------------------------- the tests

func TestEveryDefHasAGoType(t *testing.T) {
	s := loadSchema(t)
	for name := range s.Defs {
		switch name {
		case "SyncEvent", "LiveMessage":
			continue // unions: DecodeSyncEvent, DecodeLiveMessage
		}
		if newDef(name) == nil {
			t.Errorf("$def %s has no Go type", name)
		}
	}
	if newDef("NotADef") != nil {
		t.Error("newDef must give nil for a name the schema does not have")
	}
}

func TestEveryGoldenFileRoundTrips(t *testing.T) {
	s := loadSchema(t)
	methods := loadMethods(t)
	c := comparer{s.Defs}
	checked := 0
	for _, f := range goldenFiles(t) {
		for _, p := range pieces(t, f, methods) {
			raw := mustJSON(p.raw)
			if p.def == "" && strings.Contains(p.where, "invalid_request") && strings.Contains(p.where, "request") {
				// Deliberately broken calls (no or a wrong jsonrpc, a method that is not a string): the server's
				// refusals of what this client never sends. Request always encodes "jsonrpc":"2.0".
				continue
			}
			enc, v, err := roundTrip(p.def, raw)
			if err != nil {
				t.Errorf("%s (%s): decode: %v", p.where, p.def, err)
				continue
			}
			if d := c.diff("$", c.nodeFor(p.def), parse(raw), parse(enc)); len(d) > 0 {
				t.Errorf("%s (%s) does not round-trip:\n  %s", p.where, p.def, strings.Join(d, "\n  "))
			}
			// A golden is the current protocol: no part of it may fall back to an Unknown variant.
			if u := findUnknown(reflect.ValueOf(v), "$"); u != "" {
				t.Errorf("%s (%s): %s", p.where, p.def, u)
			}
			checked++
		}
	}
	if checked < 300 {
		t.Errorf("only %d golden pieces checked", checked)
	}
}

// findUnknown reports the first Unknown* union variant inside v.
func findUnknown(v reflect.Value, path string) string {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return ""
		}
		return findUnknown(v.Elem(), path)
	case reflect.Struct:
		if strings.HasPrefix(v.Type().Name(), "Unknown") {
			return fmt.Sprintf("%s decoded to %s", path, v.Type().Name())
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				if u := findUnknown(v.Field(i), path+"."+v.Type().Field(i).Name); u != "" {
					return u
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if u := findUnknown(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); u != "" {
				return u
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if u := findUnknown(v.MapIndex(k), fmt.Sprintf("%s[%v]", path, k)); u != "" {
				return u
			}
		}
	}
	return ""
}

func TestEveryGoldenEventDecodesToItsOwnType(t *testing.T) {
	methods := loadMethods(t)
	for _, f := range goldenFiles(t) {
		for _, p := range pieces(t, f, methods) {
			if p.def != "SyncEvent" {
				continue
			}
			e, err := DecodeSyncEvent(mustJSON(p.raw))
			if err != nil {
				t.Fatalf("%s: %v", p.where, err)
			}
			if _, unknown := e.(UnknownEvent); unknown {
				t.Errorf("%s: a known event type decoded to UnknownEvent", p.where)
			}
			if want := p.raw.(map[string]any)["type"]; e.EventType() != want {
				t.Errorf("%s: EventType() = %q, want %q", p.where, e.EventType(), want)
			}
		}
	}
}

func TestRoundTripCatchesALostField(t *testing.T) {
	// The comparison itself: a field the Go side drops, or adds with a value that is not its default, is caught.
	s := loadSchema(t)
	c := comparer{s.Defs}
	node := c.nodeFor("LiveClosed")
	want := parse([]byte(`{"type":"closed","reason":"TOO_SLOW","message":"slow"}`))
	if d := c.diff("$", node, want, parse([]byte(`{"type":"closed","reason":"TOO_SLOW"}`))); len(d) == 0 {
		t.Error("a dropped field with a non-default value was not caught")
	}
	if d := c.diff("$", node, want, parse([]byte(`{"type":"closed","reason":"TOO_SLOW","message":"slow","x":1}`))); len(d) == 0 {
		t.Error("an added field was not caught")
	}
	if d := c.diff("$", node, parse([]byte(`{"type":"closed","reason":"TOO_SLOW"}`)),
		parse([]byte(`{"type":"closed","reason":"TOO_SLOW","message":""}`))); len(d) != 0 {
		t.Errorf("a field holding its default must match its absence: %v", d)
	}
	if d := c.diff("$", node, parse([]byte(`{"reason":"TOO_SLOW"}`)), parse([]byte(`{"reason":"REPLACED"}`))); len(d) == 0 {
		t.Error("a changed value was not caught")
	}
}

// inject adds an unknown field to every object the schema maps to a Go struct.
func (c comparer) inject(node map[string]any, v any) any {
	for _, key := range []string{"oneOf", "anyOf"} {
		if branches, ok := node[key].([]any); ok {
			for _, b := range branches {
				if bn := c.resolve(b); c.fits(bn, v) {
					if lacksTag(bn, v) {
						// Without its tag a value is picked by exact shape, so an unknown field makes it
						// unknown (kept as sent): TestATaglessSignalBodyIsPickedOnlyWhenItsShapeIsExact.
						return c.injectBelow(bn, v)
					}
					return c.inject(bn, v)
				}
			}
			return v
		}
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			out[k] = c.inject(c.fieldNode(node, k), val)
		}
		if _, hasProps := node["properties"]; hasProps && node["additionalProperties"] != true {
			out["zz_from_a_newer_server"] = map[string]any{"nested": []any{1, "x", nil}, "type": "unknown"}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = c.inject(c.resolve(node["items"]), val)
		}
		return out
	default:
		return v
	}
}

// lacksTag: v is an object without the const field its schema branch names it by.
func lacksTag(node map[string]any, v any) bool {
	obj, ok := v.(map[string]any)
	props, _ := node["properties"].(map[string]any)
	if !ok {
		return false
	}
	for k, p := range props {
		if _, isConst := p.(map[string]any)["const"]; isConst {
			if _, present := obj[k]; !present {
				return true
			}
		}
	}
	return false
}

// injectBelow injects into v's fields but not into v itself.
func (c comparer) injectBelow(node map[string]any, v any) any {
	obj := v.(map[string]any)
	out := map[string]any{}
	for k, val := range obj {
		out[k] = c.inject(c.fieldNode(node, k), val)
	}
	return out
}

// fits: the branch of a union that v belongs to (its const tags match, its required fields are present).
func (c comparer) fits(node map[string]any, v any) bool {
	for _, key := range []string{"oneOf", "anyOf"} {
		if branches, ok := node[key].([]any); ok {
			for _, b := range branches {
				if c.fits(c.resolve(b), v) {
					return true
				}
			}
			return false
		}
	}
	obj, isObj := v.(map[string]any)
	if node["type"] == "null" {
		return v == nil
	}
	if !isObj {
		return node["type"] != "object" && node["properties"] == nil
	}
	props, _ := node["properties"].(map[string]any)
	for k, p := range props {
		if cst, ok := p.(map[string]any)["const"]; ok {
			if got, present := obj[k]; present && got != cst {
				return false
			}
		}
	}
	if req, ok := node["required"].([]any); ok {
		for _, r := range req {
			if _, ok := obj[r.(string)]; !ok {
				return false
			}
		}
	}
	return props != nil
}

func TestUnknownFieldsAreIgnored(t *testing.T) {
	s := loadSchema(t)
	methods := loadMethods(t)
	c := comparer{s.Defs}
	injected := 0
	for _, f := range goldenFiles(t) {
		for _, p := range pieces(t, f, methods) {
			if p.def == "" {
				continue
			}
			node := c.nodeFor(p.def)
			raw := mustJSON(p.raw)
			noisy := mustJSON(c.inject(node, parse(raw)))
			if bytes.Equal(noisy, raw) {
				continue
			}
			injected++
			enc, _, err := roundTrip(p.def, noisy)
			if err != nil {
				t.Errorf("%s (%s): an unknown field broke decoding: %v", p.where, p.def, err)
				continue
			}
			if d := c.diff("$", node, parse(raw), parse(enc)); len(d) > 0 {
				t.Errorf("%s (%s): with unknown fields:\n  %s", p.where, p.def, strings.Join(d, "\n  "))
			}
		}
	}
	if injected < 300 {
		t.Errorf("only %d golden pieces got unknown fields", injected)
	}
}

func TestUnknownFieldsInTheEnvelopeAreIgnored(t *testing.T) {
	var r Response
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":7,"result":{},"meta":{"x":1},"error_hint":null}`), &r); err != nil {
		t.Fatalf("response with unknown fields: %v", err)
	}
	if string(r.ID) != "7" || string(r.Result) != "{}" || r.Error != nil {
		t.Errorf("decoded %+v", r)
	}
	var e RpcError
	if err := json.Unmarshal([]byte(`{"code":-32000,"message":"m","data":{"reason":"UPGRADE_REQUIRED","hint":"x"},"trace":1}`), &e); err != nil {
		t.Fatalf("error with unknown fields: %v", err)
	}
	if e.RejectReason() != RejectReasonUpgradeRequired {
		t.Errorf("RejectReason() = %q", e.RejectReason())
	}
}

func TestAKnownFieldOfTheWrongTypeIsAnError(t *testing.T) {
	// Ignoring unknown fields does not mean accepting anything: a known field that is malformed fails.
	cases := map[string]string{
		"SyncEvent":   `{"type":"topic.deleted","project":"pj_1","seq":"nine","at":"t","topic":"tp_1","by":"mb_1"}`,
		"LiveMessage": `{"type":"closed","reason":7}`,
		"LiveHello":   `{"type":"hello","ticket":"T","cursor":"zero"}`,
		"SyncBatch":   `{"events":[{"type":"topic.deleted","seq":[]}],"cursors":{}}`,
		"Signal":      `{"id":"sg_1","body":{"type":"comment.v1","text":5}}`,
	}
	for def, raw := range cases {
		if _, _, err := roundTrip(def, []byte(raw)); err == nil {
			t.Errorf("%s: %s decoded without an error", def, raw)
		}
	}
	for _, raw := range []string{`5`, `[]`, `"x"`, `null`, `{"type":5}`, `{nope`} {
		if _, err := DecodeSyncEvent([]byte(raw)); err == nil {
			t.Errorf("DecodeSyncEvent(%s): want an error", raw)
		}
	}
}

func TestUnknownEventTypeIsSkippedNotFatal(t *testing.T) {
	future := `{"project":"pj_00000000000000000000000001","seq":12,"at":"2026-01-15T09:00:00Z","type":"vote.cast","ballot":{"x":1}}`
	e, err := DecodeSyncEvent([]byte(future))
	if err != nil {
		t.Fatalf("an unknown event type must not be an error: %v", err)
	}
	u, ok := e.(UnknownEvent)
	if !ok {
		t.Fatalf("got %T, want UnknownEvent", e)
	}
	if u.Type != "vote.cast" || u.EventType() != "vote.cast" {
		t.Errorf("Type = %q, EventType() = %q", u.Type, u.EventType())
	}
	if !bytes.Equal(u.Raw, []byte(future)) {
		t.Errorf("Raw = %s", u.Raw)
	}
	if enc := mustJSON(u); !bytes.Equal(enc, []byte(future)) {
		t.Errorf("an UnknownEvent encodes as what it was sent: %s", enc)
	}

	// A missing type is unknown too, not an error.
	if e, err := DecodeSyncEvent([]byte(`{"project":"pj_1","seq":3}`)); err != nil {
		t.Errorf("an event without a type: %v", err)
	} else if u, ok := e.(UnknownEvent); !ok || u.Type != "" {
		t.Errorf("an event without a type: %#v", e)
	}

	known := `{"project":"pj_00000000000000000000000001","seq":11,"at":"2026-01-15T09:00:00Z","type":"topic.deleted",` +
		`"topic":"tp_00000000000000000000000001","by":"mb_00000000000000000000000001"}`
	batch := `{"events":[` + known + `,` + future + `,` + known + `],"cursors":{"pj_00000000000000000000000001":12},"reset":[]}`
	var b SyncBatch
	if err := json.Unmarshal([]byte(batch), &b); err != nil {
		t.Fatalf("a sync.poll batch with an unknown event: %v", err)
	}
	if len(b.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(b.Events))
	}
	if _, ok := b.Events[0].(TopicDeletedEvent); !ok {
		t.Errorf("events[0] is %T", b.Events[0])
	}
	if _, ok := b.Events[1].(UnknownEvent); !ok {
		t.Errorf("events[1] is %T, want UnknownEvent", b.Events[1])
	}
	if b.Cursors["pj_00000000000000000000000001"] != 12 {
		t.Errorf("cursors %v", b.Cursors)
	}

	m, err := DecodeLiveMessage([]byte(`{"type":"events","events":[` + future + `],"cursor":12}`))
	if err != nil {
		t.Fatalf("a live events message with an unknown event: %v", err)
	}
	le, ok := m.(LiveEvents)
	if !ok || len(le.Events) != 1 {
		t.Fatalf("got %#v", m)
	}
	if _, ok := le.Events[0].(UnknownEvent); !ok {
		t.Errorf("live events[0] is %T", le.Events[0])
	}

	// A newer live message type, or a newer signal body, is unknown rather than fatal as well.
	if m, err := DecodeLiveMessage([]byte(`{"type":"notice","text":"hi"}`)); err != nil {
		t.Errorf("an unknown live message: %v", err)
	} else if _, ok := m.(UnknownLiveMessage); !ok {
		t.Errorf("an unknown live message decoded to %T", m)
	}
	var sig Signal
	if err := json.Unmarshal([]byte(`{"id":"sg_1","body":{"type":"poll.v1","options":["a"]}}`), &sig); err != nil {
		t.Fatalf("a signal with a newer body type: %v", err)
	}
	if _, ok := sig.Body.(UnknownSignalBody); !ok {
		t.Errorf("a newer body decoded to %T", sig.Body)
	}
}

func TestConstantsMatchExport(t *testing.T) {
	s := loadSchema(t)
	goValues := map[string]any{
		"PROTOCOL_VERSION":     ProtocolVersion,
		"MIN_CLIENT_PROTOCOL":  MinClientProtocol,
		"MAX_BATCH":            MaxBatch,
		"MAX_BODY_BYTES":       MaxBodyBytes,
		"MAX_ARTIFACT_BYTES":   MaxArtifactBytes,
		"AVATAR_MAX_BYTES":     AvatarMaxBytes,
		"MAX_MILESTONES":       MaxMilestones,
		"MAX_ACTIONS":          MaxActions,
		"MAX_AFTER":            MaxAfter,
		"LIVE_PATH":            LivePath,
		"LIVE_TICKET_SECONDS":  LiveTicketSeconds,
		"LIVE_HELLO_SECONDS":   LiveHelloSeconds,
		"LIVE_MAX_EVENTS":      LiveMaxEvents,
		"LIVE_MAX_SOCKETS":     LiveMaxSockets,
		"LIVE_CLOSE_CODE":      LiveCloseCode,
		"LIVE_PING":            LivePing,
		"LIVE_PONG":            LivePong,
		"LIVE_PING_SECONDS":    LivePingSeconds,
		"LIVE_PONG_SECONDS":    LiveSilenceSeconds,
		"HOOK_TIMEOUT":         HookTimeout,
		"RPC_PATH":             RPCPath,
		"CLIENT_HEADER":        ClientHeader,
		"CLIENT_HEADER_FORMAT": ClientHeaderFormat,
		"PROTOCOL_HEADER":      ProtocolHeader,
		"HUMAN_ACTS": map[string]any{
			"methods":              HumanActsMethods,
			"verdict_signal_kinds": HumanActsVerdictSignalKinds,
			"batch_refusal":        HumanActsBatchRefusal,
		},
		"RPC_ERROR_CODES": map[string]any{
			"PARSE_ERROR":      CodeParseError,
			"INVALID_REQUEST":  CodeInvalidRequest,
			"METHOD_NOT_FOUND": CodeMethodNotFound,
			"INVALID_PARAMS":   CodeInvalidParams,
			"INTERNAL_ERROR":   CodeInternalError,
			"RULE_FAILED":      CodeRuleFailed,
		},
	}
	c := comparer{}
	for name, want := range s.Constants {
		got, ok := goValues[name]
		if !ok {
			t.Errorf("x-constants has %s, which this test does not check: add it", name)
			continue
		}
		if d := c.diff(name, map[string]any{}, want, parse(mustJSON(got))); len(d) > 0 {
			t.Errorf("%v", d)
		}
	}
	for name := range goValues {
		if _, ok := s.Constants[name]; !ok {
			t.Errorf("%s is no longer exported", name)
		}
	}
	if LiveSilenceSeconds != LivePongSeconds {
		t.Error("LiveSilenceSeconds is LIVE_PONG_SECONDS")
	}
	if ClientName != "teamflow-go" {
		t.Errorf("ClientName = %q", ClientName)
	}

	version, err := os.ReadFile(filepath.Join(exportDir, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{fmt.Sprintf("protocol %d\n", ProtocolVersion),
		fmt.Sprintf("min_client_protocol %d\n", MinClientProtocol)} {
		if !strings.Contains(string(version), want) {
			t.Errorf("VERSION does not say %q", want)
		}
	}
}

func TestMethodsMatchExport(t *testing.T) {
	methods := loadMethods(t)
	if len(Methods) != len(methods) {
		t.Errorf("Methods has %d entries, methods.json %d", len(Methods), len(methods))
	}
	for name, m := range methods {
		got, ok := Methods[name]
		if !ok {
			t.Errorf("method %s missing", name)
			continue
		}
		want := Method{Name: name, Who: m.Who, Params: m.Params, Result: m.Result}
		if got != want {
			t.Errorf("Methods[%s] = %+v, want %+v", name, got, want)
		}
		if newDef(m.Params) == nil || newDef(m.Result) == nil {
			t.Errorf("method %s: no Go type for %s or %s", name, m.Params, m.Result)
		}
	}
	if _, ok := Methods["nope.nope"]; ok {
		t.Error("Methods has a method the export does not")
	}
}

func TestGeneratedCodeIsUpToDate(t *testing.T) {
	tmp := t.TempDir()
	if err := protocolgen.WriteAll(exportDir, tmp); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	fresh, _ := filepath.Glob(filepath.Join(tmp, "*_gen.go"))
	committed, _ := filepath.Glob("*_gen.go")
	if len(fresh) == 0 {
		t.Fatal("the generator wrote nothing")
	}
	names := func(paths []string) []string {
		out := make([]string, len(paths))
		for i, p := range paths {
			out[i] = filepath.Base(p)
		}
		sort.Strings(out)
		return out
	}
	if !reflect.DeepEqual(names(fresh), names(committed)) {
		t.Fatalf("generated files %v, committed %v", names(fresh), names(committed))
	}
	for _, f := range fresh {
		want, _ := os.ReadFile(f)
		got, err := os.ReadFile(filepath.Base(f))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s is out of date: run `go run ./tools/gen`", filepath.Base(f))
		}
	}
}

func TestATaglessSignalBodyIsPickedOnlyWhenItsShapeIsExact(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want any
	}{
		"key points": {`{"summary":"s","points":["p"],"why":"w"}`, KeyPointsBody{}},
		"artifact with points": {`{"name":"a.md","kind":"markdown","summary":"s","points":["p"],"sha256":"` +
			strings.Repeat("0", 64) + `","size":1,"media_type":"text/markdown"}`, ArtifactBody{}},
		"action result with points": {`{"action":"ai_00000000000000000000000001","outcome":"done","summary":"s","points":["p"]}`,
			ActionResultBody{}},
		"comment":                     {`{"text":"hello"}`, CommentBody{}},
		"no variant has all its keys": {`{"summary":"s","points":["p"],"bogus":1}`, UnknownSignalBody{}},
		"required fields missing":     {`{"why":"w"}`, UnknownSignalBody{}},
	}
	for name, c := range cases {
		b, err := DecodeSignalBody([]byte(c.raw))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if reflect.TypeOf(b) != reflect.TypeOf(c.want) {
			t.Errorf("%s: decoded to %T, want %T", name, b, c.want)
			continue
		}
		if _, unknown := b.(UnknownSignalBody); unknown {
			if enc := mustJSON(b); !bytes.Equal(enc, []byte(c.raw)) {
				t.Errorf("%s: an unknown body must re-encode as sent, got %s", name, enc)
			}
			continue
		}
		// Nothing the body held is lost: every key sent comes back with its value.
		sent := parse([]byte(c.raw)).(map[string]any)
		got := parse(mustJSON(b)).(map[string]any)
		for k, v := range sent {
			if !reflect.DeepEqual(got[k], v) {
				t.Errorf("%s: %s = %v after the round trip, sent %v", name, k, got[k], v)
			}
		}
	}
}

func TestConstructorsCarryTheSchemaDefaults(t *testing.T) {
	s := loadSchema(t)
	c := comparer{s.Defs}
	checked := 0
	for name, d := range s.Defs {
		node := c.resolve(d)
		props, ok := node["properties"].(map[string]any)
		if !ok || node["type"] != "object" {
			continue
		}
		v := newDef(name)
		if v == nil {
			t.Errorf("%s: no constructor", name)
			continue
		}
		enc, _ := parse(mustJSON(v)).(map[string]any)
		for key, p := range props {
			def, has := p.(map[string]any)["default"]
			if !has {
				continue
			}
			checked++
			if d := c.diff(name+"."+key, c.resolve(p), def, enc[key]); len(d) > 0 {
				t.Errorf("New%s() does not carry the default: %v", name, d)
			}
		}
	}
	if checked < 100 {
		t.Errorf("only %d defaults checked", checked)
	}
	// The cases that change meaning when left at Go's zero value.
	if !NewArtifactArchive().Archived {
		t.Error("NewArtifactArchive().Archived must default to true (an archive, not an un-archive)")
	}
	if NewSignalList().Limit < 1 {
		t.Errorf("NewSignalList().Limit = %d", NewSignalList().Limit)
	}
}

func TestARequestAlwaysSaysJSONRPC20(t *testing.T) {
	enc := parse(mustJSON(Request{ID: json.RawMessage("1"), Method: "account.me"})).(map[string]any)
	if enc["jsonrpc"] != "2.0" {
		t.Errorf("encoded %v", enc)
	}
	enc = parse(mustJSON(&Request{Method: "account.me"})).(map[string]any)
	if enc["jsonrpc"] != "2.0" {
		t.Errorf("encoded through a pointer: %v", enc)
	}
	if _, hasID := enc["id"]; hasID {
		t.Errorf("a notification has no id: %v", enc)
	}
}

func TestAValueThatFitsSeveralShapesIsUnknown(t *testing.T) {
	shapes := []shape{
		{tag: "a.v1", required: []string{"x"}, fields: []string{"type", "x", "y"}, closed: true},
		{tag: "b.v1", required: []string{"x"}, fields: []string{"type", "x", "z"}, closed: true},
		{tag: "c.v1", required: []string{"w"}, fields: []string{"w"}},
	}
	for raw, want := range map[string]string{
		`{"x":1}`:         "", // a and b both fit: never a guess
		`{"x":1,"y":2}`:   "a.v1",
		`{"x":1,"z":2}`:   "b.v1",
		`{"x":1,"q":2}`:   "", // closed variants: an unknown field fits none
		`{"w":1,"any":2}`: "c.v1",
		`{}`:              "",
		`[1]`:             "",
		`null`:            "",
	} {
		if got := pickByShape([]byte(raw), shapes); got != want {
			t.Errorf("pickByShape(%s) = %q, want %q", raw, got, want)
		}
	}
}
