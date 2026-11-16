package agent

import (
	"reflect"
	"testing"
)

func TestSplitItems(t *testing.T) {
	keys := func(items []item) []string {
		var out []string
		for _, it := range items {
			out = append(out, it.key)
		}
		return out
	}
	// JSON arrays: ids are keys.
	if got := keys(splitItems(`[{"id":"a1","subject":"hi"},{"id":2}]`)); !reflect.DeepEqual(got, []string{"id:a1", "id:2"}) {
		t.Fatalf("array: %v", got)
	}
	// The first array inside an object.
	if got := splitItems(`{"count":2,"messages":[{"message_id":"m1"},{"message_id":"m2"}]}`); len(got) != 2 || got[0].key != "id:m1" {
		t.Fatalf("object: %+v", got)
	}
	// Plain text: lines, keyed by content.
	got := splitItems("db unhealthy\n\nweb healthy\n")
	if len(got) != 2 || got[0].text != "db unhealthy" || got[0].key == got[1].key {
		t.Fatalf("lines: %+v", got)
	}
	if again := splitItems("web healthy\ndb unhealthy"); again[1].key != got[0].key {
		t.Fatal("the same line must keep its key when the order changes")
	}
	// No ids: the element's text is its key.
	a, b := splitItems(`[{"x":1}]`), splitItems(`[{"x":1},{"x":2}]`)
	if a[0].key != b[0].key || b[0].key == b[1].key {
		t.Fatal("content keys")
	}
}

func TestMatches(t *testing.T) {
	for _, c := range []struct {
		filter, text string
		want         bool
	}{
		{"", "anything", true},
		{"Unhealthy", "container db is UNHEALTHY", true},
		{"from: maya", "from: Sam", false},
		{`/from:\s*maya@/`, "From:  Maya@example.com", true},
		{`/^ok$/`, "not ok", false},
	} {
		if got := Matches(c.filter, c.text); got != c.want {
			t.Errorf("Matches(%q, %q) = %v", c.filter, c.text, got)
		}
	}
	if checkFilter("/[unclosed/") == nil {
		t.Fatal("a broken regex was accepted")
	}
}
