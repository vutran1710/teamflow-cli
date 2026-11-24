package ojson

import "testing"

func TestObjKeepsOrderAndPythonsText(t *testing.T) {
	o := Obj{{"b", 1}, {"a", []string{}}, {"c", "<x> & é"}}
	o = o.Set("a", []int{1}).Set("d", nil)
	got, err := Indent(o)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"b\": 1,\n  \"a\": [\n    1\n  ],\n  \"c\": \"<x> & é\",\n  \"d\": null\n}"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if v, ok := o.Get("c"); !ok || v != "<x> & é" {
		t.Fatalf("get %v %v", v, ok)
	}
	if _, ok := o.Get("nope"); ok {
		t.Fatal("found a missing key")
	}
	if s, _ := Indent(Obj{}); s != "{}" {
		t.Fatalf("empty: %s", s)
	}
	if _, err := Indent(Obj{{"f", func() {}}}); err == nil {
		t.Fatal("an unencodable value must fail")
	}
}
