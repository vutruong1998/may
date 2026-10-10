package dynamodb

import (
	"fmt"
	"strconv"
	"strings"
)

// This file parses the DynamoDB expression languages: conditions (also used
// for filters and key conditions), projections and updates. Placeholders are
// resolved while parsing, so the AST holds attribute names and values.

// Paths.

type pathElem struct {
	name  string
	index int
	isIdx bool
}

type path []pathElem

func (p path) String() string {
	var b strings.Builder
	for i, e := range p {
		switch {
		case e.isIdx:
			fmt.Fprintf(&b, "[%d]", e.index)
		case i > 0:
			b.WriteByte('.')
			b.WriteString(e.name)
		default:
			b.WriteString(e.name)
		}
	}
	return b.String()
}

// Operands.

type operand interface{ isOperand() }

type (
	pathOperand  struct{ p path }
	valueOperand struct{ v *AV }
	sizeOperand  struct{ p path }
	// Update-only operands.
	arithOperand struct {
		op   byte // '+' or '-'
		l, r operand
	}
	ifNotExistsOperand struct {
		p path
		v operand
	}
	listAppendOperand struct{ a, b operand }
)

func (pathOperand) isOperand()        {}
func (valueOperand) isOperand()       {}
func (sizeOperand) isOperand()        {}
func (arithOperand) isOperand()       {}
func (ifNotExistsOperand) isOperand() {}
func (listAppendOperand) isOperand()  {}

// Conditions.

type cond interface{ isCond() }

type (
	andCond     struct{ l, r cond }
	orCond      struct{ l, r cond }
	notCond     struct{ c cond }
	compareCond struct {
		op   string // = <> < <= > >=
		l, r operand
	}
	betweenCond struct{ x, lo, hi operand }
	inCond      struct {
		x    operand
		list []operand
	}
	funcCond struct {
		name string // attribute_exists, attribute_not_exists, attribute_type, begins_with, contains
		args []operand
	}
)

func (andCond) isCond()     {}
func (orCond) isCond()      {}
func (notCond) isCond()     {}
func (compareCond) isCond() {}
func (betweenCond) isCond() {}
func (inCond) isCond()      {}
func (funcCond) isCond()    {}

// Updates.

type updateAction struct {
	kind string // SET, REMOVE, ADD, DELETE
	p    path
	v    operand // nil for REMOVE
}

// Lexer.

type token struct {
	kind string // ident, name (#x), value (:x), num, op, eof
	text string
	pos  int
}

func lex(s string) ([]token, error) {
	var out []token
	i := 0
	isWord := func(c byte) bool {
		return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '#' || c == ':':
			j := i + 1
			for j < len(s) && isWord(s[j]) {
				j++
			}
			if j == i+1 {
				return nil, syntaxError(string(c), s, i)
			}
			kind := "name"
			if c == ':' {
				kind = "value"
			}
			out = append(out, token{kind, s[i:j], i})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			out = append(out, token{"num", s[i:j], i})
			i = j
		case isWord(c):
			j := i
			for j < len(s) && isWord(s[j]) {
				j++
			}
			out = append(out, token{"ident", s[i:j], i})
			i = j
		case c == '<' || c == '>':
			j := i + 1
			if j < len(s) && (s[j] == '=' || (c == '<' && s[j] == '>')) {
				j++
			}
			out = append(out, token{"op", s[i:j], i})
			i = j
		case strings.IndexByte("=(),.[]+-", c) >= 0:
			out = append(out, token{"op", string(c), i})
			i++
		default:
			return nil, syntaxError(string(c), s, i)
		}
	}
	return append(out, token{"eof", "<EOF>", len(s)}), nil
}

type syntaxErr struct{ tok, near string }

func syntaxError(tok, s string, pos int) error {
	start := pos - 10
	if start < 0 {
		start = 0
	}
	end := pos + len(tok) + 10
	if end > len(s) {
		end = len(s)
	}
	return &syntaxErr{tok, s[start:end]}
}

func (e *syntaxErr) Error() string {
	return fmt.Sprintf("Syntax error; token: \"%s\", near: \"%s\"", e.tok, e.near)
}

// Parser.

// exprContext holds the placeholder maps shared by every expression in one
// request, and records which placeholders were used.
type exprContext struct {
	names      map[string]string
	values     map[string]*AV
	usedNames  map[string]bool
	usedValues map[string]bool
}

func newExprContext(names map[string]string, values map[string]*AV) (*exprContext, error) {
	for k, v := range values {
		if err := normalize(v); err != nil {
			return nil, validation("ExpressionAttributeValues contains invalid value: %s for key %s", strings.TrimPrefix(awsMessage(err), "One or more parameter values were invalid: "), k)
		}
	}
	return &exprContext{names: names, values: values, usedNames: map[string]bool{}, usedValues: map[string]bool{}}, nil
}

// checkUnused fails if a placeholder was supplied but never referenced.
func (c *exprContext) checkUnused() error {
	var names, values []string
	for k := range c.names {
		if !c.usedNames[k] {
			names = append(names, k)
		}
	}
	for k := range c.values {
		if !c.usedValues[k] {
			values = append(values, k)
		}
	}
	if len(names) > 0 {
		return validation("Value provided in ExpressionAttributeNames unused in expressions: keys: {%s}", joinSorted(names))
	}
	if len(values) > 0 {
		return validation("Value provided in ExpressionAttributeValues unused in expressions: keys: {%s}", joinSorted(values))
	}
	return nil
}

type parser struct {
	ctx  *exprContext
	src  string
	kind string // expression parameter name, for error messages
	toks []token
	pos  int
}

func (c *exprContext) parser(kind, src string) (*parser, error) {
	if strings.TrimSpace(src) == "" {
		return nil, validation("Invalid %s: The expression can not be empty;", kind)
	}
	toks, err := lex(src)
	if err != nil {
		return nil, validation("Invalid %s: %s", kind, err)
	}
	return &parser{ctx: c, src: src, kind: kind, toks: toks}, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) fail(t token) error {
	return validation("Invalid %s: %s", p.kind, syntaxError(t.text, p.src, t.pos))
}

func (p *parser) keyword(word string) bool {
	t := p.peek()
	if t.kind == "ident" && strings.EqualFold(t.text, word) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectOp(op string) error {
	if t := p.next(); t.kind != "op" || t.text != op {
		return p.fail(t)
	}
	return nil
}

func (p *parser) end() error {
	if t := p.peek(); t.kind != "eof" {
		return p.fail(t)
	}
	return nil
}

// path := name ( "." name | "[" num "]" )*
func (p *parser) path() (path, error) {
	var out path
	name, err := p.pathName()
	if err != nil {
		return nil, err
	}
	out = append(out, pathElem{name: name})
	for {
		t := p.peek()
		if t.kind != "op" || (t.text != "." && t.text != "[") {
			return out, nil
		}
		p.next()
		if t.text == "." {
			name, err := p.pathName()
			if err != nil {
				return nil, err
			}
			out = append(out, pathElem{name: name})
			continue
		}
		n := p.next()
		if n.kind != "num" {
			return nil, p.fail(n)
		}
		idx, err := strconv.Atoi(n.text)
		if err != nil {
			return nil, p.fail(n)
		}
		if err := p.expectOp("]"); err != nil {
			return nil, err
		}
		out = append(out, pathElem{index: idx, isIdx: true})
	}
}

func (p *parser) pathName() (string, error) {
	t := p.next()
	switch t.kind {
	case "ident":
		return t.text, nil
	case "name":
		v, ok := p.ctx.names[t.text]
		if !ok {
			return "", validation("Invalid %s: An expression attribute name used in the document path is not defined; attribute name: %s", p.kind, t.text)
		}
		p.ctx.usedNames[t.text] = true
		return v, nil
	}
	return "", p.fail(t)
}

func (p *parser) value(t token) (*AV, error) {
	v, ok := p.ctx.values[t.text]
	if !ok {
		return nil, validation("Invalid %s: An expression attribute value used in expression is not defined; attribute value: %s", p.kind, t.text)
	}
	p.ctx.usedValues[t.text] = true
	return v, nil
}

// operand := value | path | size(path)
func (p *parser) operand() (operand, error) {
	t := p.peek()
	switch t.kind {
	case "value":
		p.next()
		v, err := p.value(t)
		if err != nil {
			return nil, err
		}
		return valueOperand{v}, nil
	case "ident":
		if p.toks[p.pos+1].kind == "op" && p.toks[p.pos+1].text == "(" {
			return p.function(false)
		}
		fallthrough
	case "name":
		pa, err := p.path()
		if err != nil {
			return nil, err
		}
		return pathOperand{pa}, nil
	}
	return nil, p.fail(t)
}

// function parses name(args). In conditions only size() is an operand; in
// updates if_not_exists and list_append are too.
func (p *parser) function(update bool) (operand, error) {
	t := p.next()
	name := t.text
	p.next() // (
	switch {
	case name == "size" && !update:
		pa, err := p.path()
		if err != nil {
			return nil, err
		}
		return sizeOperand{pa}, p.expectOp(")")
	case name == "if_not_exists" && update:
		pa, err := p.path()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
		v, err := p.updateOperand()
		if err != nil {
			return nil, err
		}
		return ifNotExistsOperand{pa, v}, p.expectOp(")")
	case name == "list_append" && update:
		a, err := p.updateOperand()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
		b, err := p.updateOperand()
		if err != nil {
			return nil, err
		}
		return listAppendOperand{a, b}, p.expectOp(")")
	}
	if isFunction(name) {
		return nil, validation("Invalid %s: The function is not allowed to be used this way in an expression; function: %s", p.kind, name)
	}
	return nil, validation("Invalid %s: Invalid function name; function: %s", p.kind, name)
}

func isFunction(name string) bool {
	switch name {
	case "attribute_exists", "attribute_not_exists", "attribute_type", "begins_with", "contains", "size", "if_not_exists", "list_append":
		return true
	}
	return false
}

// Conditions: or := and (OR and)*; and := not (AND not)*; not := NOT not | primary.

func (p *parser) condition() (cond, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.keyword("OR") {
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = orCond{l, r}
	}
	return l, nil
}

func (p *parser) and() (cond, error) {
	l, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.keyword("AND") {
		r, err := p.not()
		if err != nil {
			return nil, err
		}
		l = andCond{l, r}
	}
	return l, nil
}

func (p *parser) not() (cond, error) {
	if p.keyword("NOT") {
		c, err := p.not()
		if err != nil {
			return nil, err
		}
		return notCond{c}, nil
	}
	return p.primary()
}

func (p *parser) primary() (cond, error) {
	t := p.peek()
	if t.kind == "op" && t.text == "(" {
		p.next()
		c, err := p.condition()
		if err != nil {
			return nil, err
		}
		return c, p.expectOp(")")
	}
	next := p.toks[p.pos+1]
	if t.kind == "ident" && next.kind == "op" && next.text == "(" && t.text != "size" {
		return p.condFunction()
	}
	l, err := p.operand()
	if err != nil {
		return nil, err
	}
	t = p.next()
	switch {
	case t.kind == "op" && isComparator(t.text):
		r, err := p.operand()
		if err != nil {
			return nil, err
		}
		return compareCond{t.text, l, r}, nil
	case t.kind == "ident" && strings.EqualFold(t.text, "BETWEEN"):
		lo, err := p.operand()
		if err != nil {
			return nil, err
		}
		if !p.keyword("AND") {
			return nil, p.fail(p.peek())
		}
		hi, err := p.operand()
		if err != nil {
			return nil, err
		}
		return betweenCond{l, lo, hi}, nil
	case t.kind == "ident" && strings.EqualFold(t.text, "IN"):
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var list []operand
		for {
			o, err := p.operand()
			if err != nil {
				return nil, err
			}
			list = append(list, o)
			if p.peek().text != "," {
				break
			}
			p.next()
		}
		if len(list) > 100 {
			return nil, validation("Invalid %s: The IN operator is provided with too many operands; number of operands: %d", p.kind, len(list))
		}
		return inCond{l, list}, p.expectOp(")")
	}
	return nil, p.fail(t)
}

func isComparator(op string) bool {
	switch op {
	case "=", "<>", "<", "<=", ">", ">=":
		return true
	}
	return false
}

func (p *parser) condFunction() (cond, error) {
	t := p.next()
	name := t.text
	arity := map[string]int{"attribute_exists": 1, "attribute_not_exists": 1, "attribute_type": 2, "begins_with": 2, "contains": 2}
	n, ok := arity[name]
	if !ok {
		if isFunction(name) {
			return nil, validation("Invalid %s: The function is not allowed to be used this way in an expression; function: %s", p.kind, name)
		}
		return nil, validation("Invalid %s: Invalid function name; function: %s", p.kind, name)
	}
	p.next() // (
	var args []operand
	for {
		o, err := p.operand()
		if err != nil {
			return nil, err
		}
		args = append(args, o)
		if p.peek().text != "," {
			break
		}
		p.next()
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	if len(args) != n {
		return nil, validation("Invalid %s: Incorrect number of operands for operator or function; operator or function: %s, number of operands: %d", p.kind, name, len(args))
	}
	if _, isPath := args[0].(pathOperand); !isPath {
		return nil, validation("Invalid %s: Operator or function requires a document path; operator or function: %s", p.kind, name)
	}
	return funcCond{name, args}, nil
}

// parseCondition parses a condition, filter or key condition expression.
func (c *exprContext) parseCondition(kind, src string) (cond, error) {
	p, err := c.parser(kind, src)
	if err != nil {
		return nil, err
	}
	cd, err := p.condition()
	if err != nil {
		return nil, err
	}
	return cd, p.end()
}

// parseProjection parses "a, b.c, d[1]".
func (c *exprContext) parseProjection(src string) ([]path, error) {
	p, err := c.parser("ProjectionExpression", src)
	if err != nil {
		return nil, err
	}
	var out []path
	for {
		pa, err := p.path()
		if err != nil {
			return nil, err
		}
		out = append(out, pa)
		if p.peek().text != "," {
			break
		}
		p.next()
	}
	if err := p.end(); err != nil {
		return nil, err
	}
	return out, checkOverlap(out, "ProjectionExpression")
}

// parseUpdate parses SET/REMOVE/ADD/DELETE clauses, each at most once.
func (c *exprContext) parseUpdate(src string) ([]updateAction, error) {
	p, err := c.parser("UpdateExpression", src)
	if err != nil {
		return nil, err
	}
	var out []updateAction
	seen := map[string]bool{}
	for p.peek().kind != "eof" {
		t := p.next()
		kind := strings.ToUpper(t.text)
		if t.kind != "ident" || (kind != "SET" && kind != "REMOVE" && kind != "ADD" && kind != "DELETE") {
			return nil, p.fail(t)
		}
		if seen[kind] {
			return nil, validation("Invalid UpdateExpression: The \"%s\" section can only be used once in an update expression;", kind)
		}
		seen[kind] = true
		for {
			pa, err := p.path()
			if err != nil {
				return nil, err
			}
			a := updateAction{kind: kind, p: pa}
			switch kind {
			case "SET":
				if err := p.expectOp("="); err != nil {
					return nil, err
				}
				if a.v, err = p.setValue(); err != nil {
					return nil, err
				}
			case "ADD", "DELETE":
				t := p.next()
				if t.kind != "value" {
					return nil, p.fail(t)
				}
				v, err := p.value(t)
				if err != nil {
					return nil, err
				}
				a.v = valueOperand{v}
			}
			out = append(out, a)
			if p.peek().text != "," {
				break
			}
			p.next()
		}
	}
	paths := make([]path, len(out))
	for i, a := range out {
		paths[i] = a.p
	}
	return out, checkOverlap(paths, "UpdateExpression")
}

// setValue := updateOperand ( ("+" | "-") updateOperand )?
func (p *parser) setValue() (operand, error) {
	l, err := p.updateOperand()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind == "op" && (t.text == "+" || t.text == "-") {
		p.next()
		r, err := p.updateOperand()
		if err != nil {
			return nil, err
		}
		return arithOperand{t.text[0], l, r}, nil
	}
	return l, nil
}

func (p *parser) updateOperand() (operand, error) {
	t := p.peek()
	if t.kind == "ident" && p.toks[p.pos+1].text == "(" {
		return p.function(true)
	}
	return p.operand()
}

// checkOverlap rejects paths where one is a prefix of (or equal to) another.
func checkOverlap(paths []path, kind string) error {
	for i := range paths {
		for j := i + 1; j < len(paths); j++ {
			a, b := paths[i], paths[j]
			n := len(a)
			if len(b) < n {
				n = len(b)
			}
			same := true
			for k := 0; k < n; k++ {
				if a[k] != b[k] {
					same = false
					break
				}
			}
			if !same {
				continue
			}
			if len(a) == len(b) {
				return validation("Invalid %s: Two document paths conflict with each other; must remove or rewrite one of these paths; path one: %s, path two: %s", kind, pathList(a), pathList(b))
			}
			return validation("Invalid %s: Two document paths overlap with each other; must remove or rewrite one of these paths; path one: %s, path two: %s", kind, pathList(a), pathList(b))
		}
	}
	return nil
}

func pathList(p path) string {
	parts := make([]string, len(p))
	for i, e := range p {
		if e.isIdx {
			parts[i] = "[" + strconv.Itoa(e.index) + "]"
		} else {
			parts[i] = e.name
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
