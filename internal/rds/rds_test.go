package rds

import (
	"net/url"
	"testing"

	"github.com/maycloud/may/internal/awsproto"
)

func TestResolveVersion(t *testing.T) {
	pg, my := engines["postgres"], engines["mysql"]
	cases := []struct {
		e        *engine
		in, want string
		family   string
		wantErr  bool
	}{
		{pg, "", "17.4", "postgres17", false},
		{pg, "16", "16.8", "postgres16", false},
		{pg, "16.8", "16.8", "postgres16", false},
		{pg, "16.3", "16.3", "postgres16", false}, // unlisted minor of a known major
		{pg, "9.6", "", "", true},
		{pg, "1", "", "", true},
		{my, "8.0", "8.0.41", "mysql8.0", false},
		{my, "8.0.36", "8.0.36", "mysql8.0", false},
		{my, "5.7", "", "", true},
	}
	for _, c := range cases {
		v, err := c.e.resolve(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s %q: want an error, got %+v", c.e.name, c.in, v)
			}
			continue
		}
		if err != nil || v.version != c.want || v.family != c.family {
			t.Errorf("%s %q: got %+v, %v; want %s (%s)", c.e.name, c.in, v, err, c.want, c.family)
		}
	}
	if img := pg.image(engineVersion{major: "16"}); img != "postgres:16" {
		t.Errorf("image: %s", img)
	}
}

func TestParseClass(t *testing.T) {
	cases := map[string]instanceClass{
		"db.t3.micro":    {2, 1024},
		"db.t4g.medium":  {2, 4096},
		"db.m5.large":    {2, 8192},
		"db.r6g.large":   {2, 16384},
		"db.m6i.4xlarge": {16, 65536},
	}
	for c, want := range cases {
		if got, ok := parseClass(c); !ok || got != want {
			t.Errorf("%s: got %+v %v, want %+v", c, got, ok, want)
		}
	}
	for _, bad := range []string{"t3.micro", "db.t3", "db.t3.huge", "DB.T3.MICRO"} {
		if _, ok := parseClass(bad); ok {
			t.Errorf("%s: should be invalid", bad)
		}
	}
}

func TestIdentifier(t *testing.T) {
	for in, want := range map[string]string{"MyDB": "mydb", "a-1": "a-1"} {
		got, err := identifier(awsproto.Form(url.Values{"Id": {in}}), "Id", "Id")
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"1db", "db-", "a--b", "a_b", ""} {
		if _, err := identifier(awsproto.Form(url.Values{"Id": {bad}}), "Id", "Id"); err == nil {
			t.Errorf("%q: should be invalid", bad)
		}
	}
	if got := nameFromARN("arn:aws:rds:us-east-1:000000000000:db:Web", "db"); got != "web" {
		t.Errorf("nameFromARN: %q", got)
	}
}

func TestPaginate(t *testing.T) {
	items := make([]int, 45)
	for i := range items {
		items[i] = i
	}
	f := awsproto.Form(url.Values{"MaxRecords": {"20"}})
	page, marker, err := paginate(items, f)
	if err != nil || len(page) != 20 || marker != "20" {
		t.Fatalf("page 1: %v %q %v", len(page), marker, err)
	}
	f["Marker"] = []string{"40"}
	page, marker, err = paginate(items, f)
	if err != nil || len(page) != 5 || marker != "" || page[0] != 40 {
		t.Fatalf("last page: %v %q %v", page, marker, err)
	}
	if _, _, err := paginate(items, awsproto.Form(url.Values{"MaxRecords": {"5"}})); err == nil {
		t.Error("MaxRecords below 20 should fail")
	}
}
