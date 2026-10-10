package persist

import (
	"bytes"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type node struct {
	name     string
	next     *node
	created  time.Time
	data     []byte
	attrs    map[string]string
	byNum    map[int][]string
	any      interface{}
	notify   chan struct{}
	mu       sync.Mutex
	cache    []string `persist:"-"`
	kind     *kind
	floats   [2]float64
	Exported int
}

type kind struct{ name string }

var kinds = map[string]*kind{"a": {"a"}, "b": {"b"}}

type payload struct{ X, y int }

func init() {
	Intern(func(k *kind) string { return k.name }, func(n string) *kind { return kinds[n] })
	Register(payload{})
}

type memBlobs map[string][]byte

func (m memBlobs) Put(b []byte) (string, error) {
	h := Hash(b)
	m[h] = append([]byte(nil), b...)
	return h, nil
}
func (m memBlobs) Get(h string) ([]byte, error) { return m[h], nil }

func TestRoundTrip(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	big := bytes.Repeat([]byte("x"), BlobThreshold)
	shared := &node{name: "shared"}
	a := &node{name: "a", next: shared, created: when, data: []byte{0, 1, 255}, attrs: map[string]string{"$ref": "1", "k": "\xff\xfe"},
		byNum: map[int][]string{2: {"two"}, 1: nil}, any: payload{1, 2}, notify: make(chan struct{}), cache: []string{"x"},
		kind: kinds["b"], floats: [2]float64{1.5, -2}, Exported: 7}
	b := &node{name: "b", next: shared, data: big, any: "str"}
	shared.next = a // cycle
	type root struct {
		Nodes map[string]*node
		List  []*node
	}
	in := root{Nodes: map[string]*node{"a": a, "b": b}, List: []*node{shared, a}}

	blobs := memBlobs{}
	var buf bytes.Buffer
	if err := Encode(&buf, &in, blobs); err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 || strings.Contains(buf.String(), "xxxx") {
		t.Fatalf("big slice not stored as a blob: %d blobs", len(blobs))
	}
	first := buf.String()

	var out root
	if err := Decode(strings.NewReader(first), &out, blobs); err != nil {
		t.Fatal(err)
	}
	oa, ob := out.Nodes["a"], out.Nodes["b"]
	if oa.next != ob.next || out.List[0] != oa.next || out.List[1] != oa || oa.next.next != oa {
		t.Fatal("shared pointers not preserved")
	}
	if !oa.created.Equal(when) || !bytes.Equal(ob.data, big) || oa.attrs["$ref"] != "1" || oa.attrs["k"] != "\xff\xfe" {
		t.Fatalf("values differ: %+v", oa)
	}
	if !reflect.DeepEqual(oa.byNum, a.byNum) || oa.any != (payload{1, 2}) || ob.any != "str" || oa.floats != a.floats || oa.Exported != 7 {
		t.Fatalf("values differ: %+v", oa)
	}
	if oa.kind != kinds["b"] {
		t.Fatal("interned pointer not restored to the catalog entry")
	}
	if oa.cache != nil || oa.notify != nil {
		t.Fatal("skipped fields were stored")
	}

	buf.Reset()
	if err := Encode(&buf, &out, blobs); err != nil {
		t.Fatal(err)
	}
	if buf.String() != first {
		t.Fatalf("second encoding differs:\n%s\n%s", first, buf.String())
	}
}

func TestUnregisteredInterface(t *testing.T) {
	type unknown struct{ A int }
	v := struct{ x interface{} }{unknown{1}}
	if err := Encode(&bytes.Buffer{}, &v, nil); err == nil || !strings.Contains(err.Error(), "unregistered") {
		t.Fatalf("got %v", err)
	}
}
