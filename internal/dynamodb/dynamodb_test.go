package dynamodb

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormNumber(t *testing.T) {
	cases := map[string]string{
		"1": "1", "01": "1", "1.50": "1.5", "-0": "0", "0.000": "0", "1e3": "1000",
		"1.5E-3": "0.0015", ".5": "0.5", "+7": "7", "-12.340": "-12.34", "123456789012345678901234567890": "123456789012345678901234567890",
	}
	for in, want := range cases {
		if got, err := normNumber(in); err != nil || got != want {
			t.Errorf("normNumber(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "1..2", "1e", "--1", strings.Repeat("9", 39), "1e126", "1e-131"} {
		if _, err := normNumber(bad); err == nil {
			t.Errorf("normNumber(%q) succeeded", bad)
		}
	}
	if got, _ := addNumbers("0.1", "0.2", 1); got != "0.3" {
		t.Errorf("0.1 + 0.2 = %s", got)
	}
	if got, _ := addNumbers("10", "12.5", -1); got != "-2.5" {
		t.Errorf("10 - 12.5 = %s", got)
	}
}

func TestAVJSON(t *testing.T) {
	src := `{"a":{"S":"x"},"b":{"N":"1"},"c":{"L":[]},"d":{"M":{}},"e":{"B":"AAE="},"f":{"NULL":true},"g":{"SS":["p","q"]}}`
	var item Item
	if err := json.Unmarshal([]byte(src), &item); err != nil {
		t.Fatal(err)
	}
	if err := normalizeItem(item); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(item)
	if string(out) != src {
		t.Fatalf("round trip:\n got %s\nwant %s", out, src)
	}
	var bad Item
	_ = json.Unmarshal([]byte(`{"a":{"S":"x","N":"1"}}`), &bad)
	if err := normalizeItem(bad); err == nil {
		t.Fatal("two types accepted")
	}
	_ = json.Unmarshal([]byte(`{"a":{"SS":[]}}`), &bad)
	if err := normalizeItem(bad); err == nil {
		t.Fatal("empty set accepted")
	}
}

func mustItem(t *testing.T, s string) Item {
	t.Helper()
	var item Item
	if err := json.Unmarshal([]byte(s), &item); err != nil {
		t.Fatal(err)
	}
	if err := normalizeItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

func ctxFor(t *testing.T, names map[string]string, values string) *exprContext {
	t.Helper()
	var vals map[string]*AV
	if values != "" {
		if err := json.Unmarshal([]byte(values), &vals); err != nil {
			t.Fatal(err)
		}
	}
	ctx, err := newExprContext(names, vals)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestConditions(t *testing.T) {
	item := mustItem(t, `{"pk":{"S":"a"},"n":{"N":"5"},"s":{"S":"hello"},"tags":{"SS":["x","y"]},
		"list":{"L":[{"S":"p"},{"N":"2"}]},"m":{"M":{"k":{"S":"v"},"deep":{"L":[{"N":"1"}]}}},"empty":{"S":""}}`)
	values := `{":five":{"N":"5"},":four":{"N":"4.0"},":six":{"N":"6"},":hel":{"S":"hel"},":x":{"S":"x"},
		":p":{"S":"p"},":v":{"S":"v"},":ss":{"S":"SS"},":two":{"N":"2"},":z":{"S":"z"}}`
	cases := []struct {
		expr string
		want bool
	}{
		{"n = :five", true},
		{"n <> :five", false},
		{"missing <> :five", true},
		{"missing = :five", false},
		{"n > :four AND n < :six", true},
		{"n BETWEEN :four AND :six", true},
		{"n BETWEEN :six AND :six", false},
		{"n IN (:four, :five)", true},
		{"NOT n IN (:four, :six)", true},
		{"begins_with(s, :hel)", true},
		{"contains(s, :hel)", true},
		{"contains(tags, :x)", true},
		{"contains(tags, :z)", false},
		{"contains(list, :p)", true},
		{"attribute_exists(m.k) AND attribute_not_exists(m.nope)", true},
		{"m.k = :v", true},
		{"m.deep[0] = :five", false},
		{"list[1] = :two", true},
		{"size(s) = :five", true},
		{"size(tags) = :two", true},
		{"attribute_type(tags, :ss)", true},
		{"(n = :six OR s = :v) OR #n = :five", true},
		{"n < :hel", false}, // different types never compare
		{"attribute_exists(empty)", true},
	}
	for _, c := range cases {
		ctx := ctxFor(t, map[string]string{"#n": "n"}, values)
		cd, err := ctx.parseCondition("ConditionExpression", c.expr)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := evalCond(item, cd); got != c.want {
			t.Errorf("%s = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestConditionErrors(t *testing.T) {
	cases := map[string]string{
		"a =":                     "Syntax error",
		"a = :missing":            "attribute value used in expression is not defined",
		"#missing = :v":           "attribute name used in the document path is not defined",
		"nope(a)":                 "Invalid function name",
		"attribute_exists(a, :v)": "Incorrect number of operands",
		"a = :v extra":            "Syntax error",
		"":                        "The expression can not be empty",
	}
	for expr, want := range cases {
		ctx := ctxFor(t, nil, `{":v":{"S":"x"}}`)
		_, err := ctx.parseCondition("ConditionExpression", expr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", expr, err, want)
		}
	}
	ctx := ctxFor(t, map[string]string{"#a": "a"}, `{":v":{"S":"x"},":unused":{"S":"y"}}`)
	if _, err := ctx.parseCondition("ConditionExpression", "#a = :v"); err != nil {
		t.Fatal(err)
	}
	if err := ctx.checkUnused(); err == nil || !strings.Contains(err.Error(), ":unused") {
		t.Fatalf("unused value: %v", err)
	}
}

func update(t *testing.T, item Item, expr, values string) (Item, error) {
	t.Helper()
	ctx := ctxFor(t, map[string]string{"#c": "count"}, values)
	actions, err := ctx.parseUpdate(expr)
	if err != nil {
		return nil, err
	}
	return applyUpdate(item, actions)
}

func TestUpdates(t *testing.T) {
	base := mustItem(t, `{"pk":{"S":"a"},"n":{"N":"5"},"l":{"L":[{"N":"0"},{"N":"1"},{"N":"2"},{"N":"3"}]},
		"m":{"M":{"k":{"S":"v"}}},"ss":{"SS":["a","b"]}}`)
	values := `{":one":{"N":"1"},":l":{"L":[{"S":"z"}]},":s":{"SS":["b","c"]},":a":{"SS":["a"]},":v":{"S":"new"}}`
	got, err := update(t, base, "SET n = n + :one, #c = if_not_exists(#c, :one), l = list_append(l, :l), m.k2 = :v ADD ss :s", values)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	for _, want := range []string{`"n":{"N":"6"}`, `"count":{"N":"1"}`, `{"S":"z"}]`, `"k2":{"S":"new"}`, `"ss":{"SS":["a","b","c"]}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	if _, ok := base["count"]; ok {
		t.Fatal("update mutated the original item")
	}

	// REMOVE of several list elements refers to the original indexes.
	got, err = update(t, base, "REMOVE l[0], l[2] DELETE ss :a", values)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(got["l"]); string(b) != `{"L":[{"N":"1"},{"N":"3"}]}` {
		t.Errorf("list after REMOVE = %s", b)
	}
	if b, _ := json.Marshal(got["ss"]); string(b) != `{"SS":["b"]}` {
		t.Errorf("set after DELETE = %s", b)
	}

	errs := map[string]string{
		"SET nope.x = :v":           "document path provided in the update expression is invalid",
		"SET n = :v, n = :one":      "Two document paths conflict",
		"SET m = :v REMOVE m.k":     "Two document paths overlap",
		"SET n = missing + :one":    "does not exist in the item",
		"ADD m :one":                "incorrect data type",
		"SET a = :one SET b = :one": "can only be used once",
	}
	for expr, want := range errs {
		_, err := update(t, base, expr, `{":v":{"S":"x"},":one":{"N":"1"}}`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", expr, err, want)
		}
	}
}

func TestProjection(t *testing.T) {
	item := mustItem(t, `{"a":{"S":"1"},"b":{"M":{"c":{"S":"2"},"d":{"S":"3"}}},"l":{"L":[{"S":"x"},{"S":"y"},{"S":"z"}]}}`)
	ctx := ctxFor(t, nil, "")
	paths, err := ctx.parseProjection("a, b.c, l[2], l[0], missing")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(project(item, paths))
	if want := `{"a":{"S":"1"},"b":{"M":{"c":{"S":"2"}}},"l":{"L":[{"S":"x"},{"S":"z"}]}}`; string(b) != want {
		t.Fatalf("project = %s, want %s", b, want)
	}
}
