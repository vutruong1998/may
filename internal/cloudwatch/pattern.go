package cloudwatch

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// A pattern is a compiled CloudWatch Logs filter pattern. Three syntaxes
// exist: terms ("ERROR -debug ?WARN"), JSON ({ $.level = "error" }) and
// space-delimited ([ip, user, ..., status = 5*]).
type pattern interface{ match(msg string) bool }

func errPattern(p string) error {
	return invalidParam("Invalid filter pattern: %s", p)
}

func parsePattern(p string) (pattern, error) {
	t := strings.TrimSpace(p)
	switch {
	case t == "" || t == `""`:
		return matchAll{}, nil
	case strings.HasPrefix(t, "{"):
		if !strings.HasSuffix(t, "}") {
			return nil, errPattern(p)
		}
		return parseJSONPattern(t[1:len(t)-1], p)
	case strings.HasPrefix(t, "["):
		if !strings.HasSuffix(t, "]") {
			return nil, errPattern(p)
		}
		return parseDelimited(t[1:len(t)-1], p)
	}
	return parseTerms(t, p)
}

type matchAll struct{}

func (matchAll) match(string) bool { return true }

// Terms.

type termMatcher struct {
	text  string
	regex *regexp.Regexp
}

func (t termMatcher) in(msg string) bool {
	if t.regex != nil {
		return t.regex.MatchString(msg)
	}
	return strings.Contains(msg, t.text)
}

type termPattern struct {
	required, excluded, optional []termMatcher
}

func (p termPattern) match(msg string) bool {
	for _, t := range p.required {
		if !t.in(msg) {
			return false
		}
	}
	for _, t := range p.excluded {
		if t.in(msg) {
			return false
		}
	}
	if len(p.optional) == 0 {
		return true
	}
	for _, t := range p.optional {
		if t.in(msg) {
			return true
		}
	}
	return false
}

// splitTerms splits on spaces, keeping "quoted phrases" and %regex% whole.
func splitTerms(s string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '"' || c == '%':
			quote = c
			cur.WriteByte(c)
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, false
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, true
}

func parseTerms(t, orig string) (pattern, error) {
	words, ok := splitTerms(t)
	if !ok {
		return nil, errPattern(orig)
	}
	var p termPattern
	for _, w := range words {
		list := &p.required
		switch w[0] {
		case '-':
			list, w = &p.excluded, w[1:]
		case '?':
			list, w = &p.optional, w[1:]
		}
		var m termMatcher
		switch {
		case len(w) >= 2 && w[0] == '%' && w[len(w)-1] == '%':
			re, err := regexp.Compile(w[1 : len(w)-1])
			if err != nil {
				return nil, errPattern(orig)
			}
			m.regex = re
		case len(w) >= 2 && w[0] == '"' && w[len(w)-1] == '"':
			m.text = w[1 : len(w)-1]
		case w == "":
			return nil, errPattern(orig)
		default:
			m.text = w
		}
		*list = append(*list, m)
	}
	return p, nil
}

// Shared comparison of a field value with a pattern literal.

type comparison struct {
	op    string // = != < <= > >=
	value string
	num   *float64
	isStr bool           // quoted string literal
	re    *regexp.Regexp // value with * wildcards
}

// wildcard turns a literal with * into a regexp anchored at both ends.
func wildcard(s string) *regexp.Regexp {
	parts := strings.Split(s, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}

func (c comparison) test(field string, present bool) bool {
	if !present {
		return c.op == "!="
	}
	if c.num != nil {
		f, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return c.op == "!="
		}
		switch c.op {
		case "=":
			return f == *c.num
		case "!=":
			return f != *c.num
		case "<":
			return f < *c.num
		case "<=":
			return f <= *c.num
		case ">":
			return f > *c.num
		case ">=":
			return f >= *c.num
		}
		return false
	}
	eq := c.re.MatchString(field)
	if c.op == "!=" {
		return !eq
	}
	return c.op == "=" && eq
}

// Tokenizer shared by JSON and space-delimited patterns.

type ptoken struct {
	kind string // sel, str, num, word, op, lp, rp, and, or, comma
	text string
}

var operators = []string{"&&", "||", "!=", "<=", ">=", "=", "<", ">"}

func tokenize(s string) ([]ptoken, bool) {
	var out []ptoken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(':
			out = append(out, ptoken{"lp", "("})
			i++
		case c == ')':
			out = append(out, ptoken{"rp", ")"})
			i++
		case c == ',':
			out = append(out, ptoken{"comma", ","})
			i++
		case c == '"':
			j := i + 1
			var b strings.Builder
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, false
			}
			out = append(out, ptoken{"str", b.String()})
			i = j + 1
		default:
			matched := false
			for _, op := range operators {
				if strings.HasPrefix(s[i:], op) {
					kind := "op"
					if op == "&&" {
						kind = "and"
					} else if op == "||" {
						kind = "or"
					}
					out = append(out, ptoken{kind, op})
					i += len(op)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n(),\"&|!=<>", rune(s[j])) {
				j++
			}
			if j == i {
				return nil, false
			}
			w := s[i:j]
			kind := "word"
			if strings.HasPrefix(w, "$") {
				kind = "sel"
			} else if _, err := strconv.ParseFloat(w, 64); err == nil {
				kind = "num"
			}
			out = append(out, ptoken{kind, w})
			i = j
		}
	}
	return out, true
}

// JSON patterns.

type jsonNode interface {
	eval(doc interface{}) bool
}

type jsonAnd struct{ l, r jsonNode }
type jsonOr struct{ l, r jsonNode }

func (n jsonAnd) eval(d interface{}) bool { return n.l.eval(d) && n.r.eval(d) }
func (n jsonOr) eval(d interface{}) bool  { return n.l.eval(d) || n.r.eval(d) }

type jsonTest struct {
	path []string // field names; "[n]" for indexes
	cmp  *comparison
	is   string // TRUE, FALSE, NULL, NOT EXISTS
}

func lookup(doc interface{}, path []string) (interface{}, bool) {
	cur := doc
	for _, p := range path {
		if strings.HasPrefix(p, "[") {
			i, _ := strconv.Atoi(p[1 : len(p)-1])
			arr, ok := cur.([]interface{})
			if !ok || i >= len(arr) {
				return nil, false
			}
			cur = arr[i]
			continue
		}
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		if cur, ok = obj[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func (t jsonTest) eval(doc interface{}) bool {
	v, ok := lookup(doc, t.path)
	switch t.is {
	case "NOT EXISTS":
		return !ok
	case "TRUE":
		return ok && v == true
	case "FALSE":
		return ok && v == false
	case "NULL":
		return ok && v == nil
	}
	if !ok {
		return t.cmp.op == "!="
	}
	var field string
	switch x := v.(type) {
	case string:
		if t.cmp.num != nil {
			return t.cmp.op == "!="
		}
		field = x
	case float64:
		if t.cmp.isStr {
			return t.cmp.op == "!="
		}
		field = strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		field = strconv.FormatBool(x)
	case nil:
		field = "null"
	default:
		return t.cmp.op == "!="
	}
	return t.cmp.test(field, true)
}

type jsonPattern struct{ root jsonNode }

func (p jsonPattern) match(msg string) bool {
	var doc interface{}
	if json.Unmarshal([]byte(msg), &doc) != nil {
		return false
	}
	return p.root.eval(doc)
}

var selectorPart = regexp.MustCompile(`^[A-Za-z0-9_@\-]+$|^\[\d+\]$`)

func parseSelector(sel string) ([]string, bool) {
	rest := strings.TrimPrefix(sel, "$")
	var path []string
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			j := strings.IndexAny(rest, ".[")
			if j < 0 {
				j = len(rest)
			}
			path = append(path, rest[:j])
			rest = rest[j:]
		case strings.HasPrefix(rest, "["):
			j := strings.IndexByte(rest, ']')
			if j < 0 {
				return nil, false
			}
			path = append(path, rest[:j+1])
			rest = rest[j+1:]
		default:
			return nil, false
		}
	}
	for _, p := range path {
		if !selectorPart.MatchString(p) {
			return nil, false
		}
	}
	return path, len(path) > 0
}

type tokenStream struct {
	toks []ptoken
	pos  int
}

func (ts *tokenStream) peek() ptoken {
	if ts.pos < len(ts.toks) {
		return ts.toks[ts.pos]
	}
	return ptoken{kind: "eof"}
}

func (ts *tokenStream) next() ptoken {
	t := ts.peek()
	ts.pos++
	return t
}

func literal(t ptoken) (*comparison, bool) {
	switch t.kind {
	case "str":
		return &comparison{value: t.text, isStr: true, re: wildcard(t.text)}, true
	case "num":
		f, _ := strconv.ParseFloat(t.text, 64)
		return &comparison{value: t.text, num: &f, re: wildcard(t.text)}, true
	case "word":
		return &comparison{value: t.text, re: wildcard(t.text)}, true
	}
	return nil, false
}

func parseJSONPattern(body, orig string) (pattern, error) {
	toks, ok := tokenize(body)
	if !ok {
		return nil, errPattern(orig)
	}
	ts := &tokenStream{toks: toks}
	var expr func() (jsonNode, bool)
	var primary func() (jsonNode, bool)
	primary = func() (jsonNode, bool) {
		t := ts.next()
		if t.kind == "lp" {
			n, ok := expr()
			if !ok || ts.next().kind != "rp" {
				return nil, false
			}
			return n, true
		}
		if t.kind != "sel" {
			return nil, false
		}
		path, ok := parseSelector(t.text)
		if !ok {
			return nil, false
		}
		op := ts.next()
		if op.kind == "word" && strings.EqualFold(op.text, "IS") {
			w := ts.next()
			return jsonTest{path: path, is: strings.ToUpper(w.text)}, w.kind == "word" &&
				(strings.EqualFold(w.text, "TRUE") || strings.EqualFold(w.text, "FALSE") || strings.EqualFold(w.text, "NULL"))
		}
		if op.kind == "word" && strings.EqualFold(op.text, "NOT") {
			w := ts.next()
			return jsonTest{path: path, is: "NOT EXISTS"}, strings.EqualFold(w.text, "EXISTS")
		}
		if op.kind != "op" {
			return nil, false
		}
		c, ok := literal(ts.next())
		if !ok {
			return nil, false
		}
		c.op = op.text
		return jsonTest{path: path, cmp: c}, true
	}
	expr = func() (jsonNode, bool) {
		l, ok := primary()
		if !ok {
			return nil, false
		}
		for {
			switch ts.peek().kind {
			case "and":
				ts.next()
				r, ok := primary()
				if !ok {
					return nil, false
				}
				l = jsonAnd{l, r}
			case "or":
				ts.next()
				r, ok := expr()
				if !ok {
					return nil, false
				}
				return jsonOr{l, r}, true
			default:
				return l, true
			}
		}
	}
	root, ok := expr()
	if !ok || ts.peek().kind != "eof" {
		return nil, errPattern(orig)
	}
	return jsonPattern{root}, nil
}

// Space-delimited patterns.

type delimField struct {
	name     string
	ellipsis bool
	conds    [][]comparison // OR of ANDs
}

type delimPattern struct{ fields []delimField }

// splitFields splits a log line on spaces, treating "quoted" and
// [bracketed] text as single fields.
func splitFields(msg string) []string {
	var out []string
	for i := 0; i < len(msg); {
		switch msg[i] {
		case ' ', '\t':
			i++
		case '"', '[':
			end := byte('"')
			if msg[i] == '[' {
				end = ']'
			}
			j := strings.IndexByte(msg[i+1:], end)
			if j < 0 {
				out = append(out, msg[i+1:])
				return out
			}
			out = append(out, msg[i+1:i+1+j])
			i += j + 2
		default:
			j := strings.IndexAny(msg[i:], " \t")
			if j < 0 {
				out = append(out, msg[i:])
				return out
			}
			out = append(out, msg[i:i+j])
			i += j
		}
	}
	return out
}

func (f delimField) test(v string) bool {
	if len(f.conds) == 0 {
		return true
	}
	for _, and := range f.conds {
		ok := true
		for _, c := range and {
			if !c.test(v, true) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func (p delimPattern) match(msg string) bool {
	return matchFields(p.fields, splitFields(msg))
}

func matchFields(pattern []delimField, values []string) bool {
	if len(pattern) == 0 {
		return len(values) == 0
	}
	f := pattern[0]
	if f.ellipsis {
		for skip := 0; skip <= len(values); skip++ {
			if matchFields(pattern[1:], values[skip:]) {
				return true
			}
		}
		return false
	}
	if len(values) == 0 {
		return false
	}
	// The last named field absorbs any remaining values.
	if len(pattern) == 1 && len(values) > 1 {
		return f.test(strings.Join(values, " "))
	}
	return f.test(values[0]) && matchFields(pattern[1:], values[1:])
}

func parseDelimited(body, orig string) (pattern, error) {
	toks, ok := tokenize(body)
	if !ok {
		return nil, errPattern(orig)
	}
	var fields []delimField
	ts := &tokenStream{toks: toks}
	for ts.peek().kind != "eof" {
		t := ts.next()
		if t.kind != "word" {
			return nil, errPattern(orig)
		}
		f := delimField{name: t.text, ellipsis: t.text == "..."}
		var and []comparison
		for ts.peek().kind == "op" {
			op := ts.next()
			c, ok := literal(ts.next())
			if !ok {
				return nil, errPattern(orig)
			}
			c.op = op.text
			and = append(and, *c)
			switch ts.peek().kind {
			case "and":
				ts.next()
				if ts.next().text != f.name {
					return nil, errPattern(orig)
				}
			case "or":
				ts.next()
				f.conds = append(f.conds, and)
				and = nil
				if ts.next().text != f.name {
					return nil, errPattern(orig)
				}
			}
		}
		if and != nil {
			f.conds = append(f.conds, and)
		}
		fields = append(fields, f)
		if k := ts.peek().kind; k == "comma" {
			ts.next()
		} else if k != "eof" {
			return nil, errPattern(orig)
		}
	}
	if len(fields) == 0 {
		return nil, errPattern(orig)
	}
	return delimPattern{fields}, nil
}
