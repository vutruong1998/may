package cloudwatch

import "testing"

func TestPatterns(t *testing.T) {
	cases := []struct {
		pattern, msg string
		want         bool
	}{
		{"", "anything", true},
		{"ERROR", "an ERROR happened", true},
		{"ERROR", "an error happened", false},
		{"ERROR timeout", "ERROR: db timeout", true},
		{"ERROR timeout", "ERROR: db down", false},
		{`"db down"`, "ERROR: db down now", true},
		{"ERROR -retry", "ERROR will retry", false},
		{"?ERROR ?WARN", "WARN disk", true},
		{"?ERROR ?WARN", "INFO ok", false},
		{"%[0-9]{3}ms%", "took 250ms", true},
		{`{ $.level = "error" }`, `{"level":"error","ms":12}`, true},
		{`{ $.level = "err*" }`, `{"level":"error"}`, true},
		{`{ $.level != "error" }`, `{"level":"info"}`, true},
		{`{ $.ms > 100 }`, `{"ms":250}`, true},
		{`{ $.ms > 100 }`, `{"ms":50}`, false},
		{`{ $.ms > 100 }`, `not json`, false},
		{`{ $.user.id = 7 && $.ok IS TRUE }`, `{"user":{"id":7},"ok":true}`, true},
		{`{ ($.a = 1 || $.b = 2) && $.c NOT EXISTS }`, `{"b":2}`, true},
		{`{ $.items[1].sku = "b" }`, `{"items":[{"sku":"a"},{"sku":"b"}]}`, true},
		{`[ip, user, status = 5*, bytes > 1000]`, `10.0.0.1 alice 503 2048`, true},
		{`[ip, user, status = 5*, bytes > 1000]`, `10.0.0.1 alice 200 2048`, false},
		{`[..., status = 404 || status = 500, size]`, `GET /x "Mozilla 5" 404 12`, true},
		{`[ip, ts, request, status = 200]`, `1.2.3.4 [10/Oct/2026:13:55:36] "GET / HTTP/1.1" 200`, true},
	}
	for _, c := range cases {
		p, err := parsePattern(c.pattern)
		if err != nil {
			t.Errorf("%q: %v", c.pattern, err)
			continue
		}
		if got := p.match(c.msg); got != c.want {
			t.Errorf("%q on %q = %v, want %v", c.pattern, c.msg, got, c.want)
		}
	}
	for _, bad := range []string{`{ $.a = }`, `{ $.a = 1`, `[a, b = ]`, `"unterminated`, `%[%`} {
		if _, err := parsePattern(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
