package s3

import (
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestListKeys(t *testing.T) {
	b := &bucket{objects: map[string]*object{}}
	for _, k := range []string{"a/1", "a/2", "a/b/3", "a/", "b/4", "c"} {
		b.objects[k] = &object{key: k}
	}
	names := func(es []listEntry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.name())
		}
		return out
	}
	cases := []struct {
		prefix, delim, marker string
		max                   int
		want                  []string
		truncated             bool
	}{
		{"", "", "", 1000, []string{"a/", "a/1", "a/2", "a/b/3", "b/4", "c"}, false},
		{"", "/", "", 1000, []string{"a/", "b/", "c"}, false},
		{"", "/", "", 1, []string{"a/"}, true},
		{"", "/", "a/", 1000, []string{"b/", "c"}, false}, // marker is a common prefix
		{"a/", "/", "", 1000, []string{"a/", "a/1", "a/2", "a/b/"}, false},
		{"a/", "/", "a/", 1000, []string{"a/1", "a/2", "a/b/"}, false}, // marker is a key, not a prefix
		{"a/", "", "a/1", 2, []string{"a/2", "a/b/3"}, false},
	}
	for _, c := range cases {
		got, tr := listKeys(b, c.prefix, c.delim, c.marker, c.max)
		if !reflect.DeepEqual(names(got), c.want) || tr != c.truncated {
			t.Errorf("listKeys(%q,%q,%q,%d) = %v,%v; want %v,%v", c.prefix, c.delim, c.marker, c.max, names(got), tr, c.want, c.truncated)
		}
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		h          string
		start, end int
		ok, err    bool
	}{
		{"bytes=0-4", 0, 4, true, false},
		{"bytes=5-", 5, 9, true, false},
		{"bytes=-3", 7, 9, true, false},
		{"bytes=-30", 0, 9, true, false},
		{"bytes=8-100", 8, 9, true, false},
		{"bytes=10-12", 0, 0, false, true},
		{"bytes=0-1,3-4", 0, 0, false, false},
		{"bytes=5-2", 0, 0, false, false},
		{"items=0-1", 0, 0, false, false},
	}
	for _, c := range cases {
		s, e, ok, err := parseRange(c.h, 10)
		if s != c.start || e != c.end || ok != c.ok || (err != nil) != c.err {
			t.Errorf("parseRange(%q) = %d,%d,%v,%v", c.h, s, e, ok, err)
		}
	}
}

func TestParsePath(t *testing.T) {
	s := New("us-east-1", "example.test")
	cases := []struct{ host, path, bucket, key string }{
		{"localhost:4566", "/b/k/x", "b", "k/x"},
		{"localhost:4566", "/b", "b", ""},
		{"127.0.0.1:4566", "/b/k", "b", "k"},
		{"b.localhost:4566", "/k/x", "b", "k/x"},
		{"my.bucket.s3.localhost.localstack.cloud:4566", "/k", "my.bucket", "k"},
		{"b.s3.us-west-2.amazonaws.com", "/k", "b", "k"},
		{"b.example.test", "/", "b", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		r.Host = c.host
		if b, k := s.parsePath(r); b != c.bucket || k != c.key {
			t.Errorf("parsePath(%s%s) = %q,%q; want %q,%q", c.host, c.path, b, k, c.bucket, c.key)
		}
	}
}
