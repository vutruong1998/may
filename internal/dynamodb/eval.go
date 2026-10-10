package dynamodb

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// resolve finds the value at a document path.
func resolve(item Item, p path) *AV {
	if len(p) == 0 {
		return nil
	}
	v := item[p[0].name]
	for _, e := range p[1:] {
		switch {
		case v == nil:
			return nil
		case e.isIdx:
			if v.L == nil || e.index >= len(v.L) {
				return nil
			}
			v = v.L[e.index]
		default:
			if v.M == nil {
				return nil
			}
			v = v.M[e.name]
		}
	}
	return v
}

// evalOperand returns the operand's value, or nil if it refers to a missing
// attribute. Update-only operands are evaluated by evalUpdateOperand.
func evalOperand(item Item, o operand) *AV {
	switch o := o.(type) {
	case valueOperand:
		return o.v
	case pathOperand:
		return resolve(item, o.p)
	case sizeOperand:
		v := resolve(item, o.p)
		if v == nil {
			return nil
		}
		n, ok := size(v)
		if !ok {
			return nil
		}
		return num(strconv.Itoa(n))
	}
	return nil
}

func size(v *AV) (int, bool) {
	switch v.Type() {
	case "S":
		return utf8.RuneCountInString(*v.S), true
	case "B":
		return len(v.B), true
	case "SS", "NS", "BS":
		return len(v.SS) + len(v.NS) + len(v.BS), true
	case "L":
		return len(v.L), true
	case "M":
		return len(v.M), true
	}
	return 0, false
}

// evalCond evaluates a condition against an item (which may be nil).
func evalCond(item Item, c cond) bool {
	switch c := c.(type) {
	case andCond:
		return evalCond(item, c.l) && evalCond(item, c.r)
	case orCond:
		return evalCond(item, c.l) || evalCond(item, c.r)
	case notCond:
		return !evalCond(item, c.c)
	case compareCond:
		l, r := evalOperand(item, c.l), evalOperand(item, c.r)
		if c.op == "<>" {
			// A missing attribute is not equal to anything.
			return l == nil || r == nil || !equal(l, r)
		}
		if l == nil || r == nil {
			return false
		}
		if c.op == "=" {
			return equal(l, r)
		}
		cmp, ok := compare(l, r)
		if !ok {
			return false
		}
		switch c.op {
		case "<":
			return cmp < 0
		case "<=":
			return cmp <= 0
		case ">":
			return cmp > 0
		case ">=":
			return cmp >= 0
		}
	case betweenCond:
		x, lo, hi := evalOperand(item, c.x), evalOperand(item, c.lo), evalOperand(item, c.hi)
		a, ok1 := compare(x, lo)
		b, ok2 := compare(x, hi)
		return ok1 && ok2 && a >= 0 && b <= 0
	case inCond:
		x := evalOperand(item, c.x)
		for _, o := range c.list {
			if x != nil && equal(x, evalOperand(item, o)) {
				return true
			}
		}
		return false
	case funcCond:
		return evalFunc(item, c)
	}
	return false
}

func evalFunc(item Item, c funcCond) bool {
	target := evalOperand(item, c.args[0])
	switch c.name {
	case "attribute_exists":
		return target != nil
	case "attribute_not_exists":
		return target == nil
	}
	arg := evalOperand(item, c.args[1])
	if target == nil || arg == nil {
		return false
	}
	switch c.name {
	case "attribute_type":
		return arg.S != nil && target.Type() == *arg.S
	case "begins_with":
		switch {
		case target.S != nil && arg.S != nil:
			return strings.HasPrefix(*target.S, *arg.S)
		case target.B != nil && arg.B != nil:
			return bytes.HasPrefix(target.B, arg.B)
		}
	case "contains":
		switch target.Type() {
		case "S":
			return arg.S != nil && strings.Contains(*target.S, *arg.S)
		case "B":
			return arg.B != nil && bytes.Contains(target.B, arg.B)
		case "SS", "NS", "BS":
			if (target.SS != nil && arg.S == nil) || (target.NS != nil && arg.N == nil) || (target.BS != nil && arg.B == nil) {
				return false
			}
			return setKeys(target)[keyString(arg)[1:]] || (arg.N != nil && setKeys(target)[rat(*arg.N).RatString()])
		case "L":
			for _, e := range target.L {
				if equal(e, arg) {
					return true
				}
			}
		}
	}
	return false
}

// validateCondOperands rejects functions whose constant arguments have the
// wrong type, as DynamoDB does when parsing.
func validateCond(c cond, kind string) error {
	switch c := c.(type) {
	case andCond:
		if err := validateCond(c.l, kind); err != nil {
			return err
		}
		return validateCond(c.r, kind)
	case orCond:
		if err := validateCond(c.l, kind); err != nil {
			return err
		}
		return validateCond(c.r, kind)
	case notCond:
		return validateCond(c.c, kind)
	case funcCond:
		if c.name == "attribute_type" {
			v, ok := c.args[1].(valueOperand)
			if !ok || v.v.S == nil || !validType(*v.v.S) {
				got := ""
				if ok && v.v.S != nil {
					got = *v.v.S
				}
				return validation("Invalid %s: Invalid attribute type name found; type: %s, valid types: { B,NULL,SS,BOOL,L,BS,N,NS,S,M };", kind, got)
			}
		}
		if c.name == "begins_with" {
			if v, ok := c.args[1].(valueOperand); ok && v.v.S == nil && v.v.B == nil {
				return validation("Invalid %s: Incorrect operand type for operator or function; operator or function: begins_with, operand type: %s", kind, v.v.Type())
			}
		}
	}
	return nil
}

func validType(t string) bool {
	switch t {
	case "S", "N", "B", "BOOL", "NULL", "SS", "NS", "BS", "L", "M":
		return true
	}
	return false
}

// Projection.

// project returns an item holding only the given paths. Paths are merged
// into a mask first, so list elements keep their original order and paths
// sharing a prefix land in the same nested value.
func project(item Item, paths []path) Item {
	if item == nil {
		return nil
	}
	root := &mask{}
	for _, p := range paths {
		root.add(p)
	}
	out := Item{}
	for name, m := range root.fields {
		if v := m.apply(item[name]); v != nil {
			out[name] = v
		}
	}
	return out
}

type mask struct {
	all    bool
	fields map[string]*mask
	elems  map[int]*mask
}

func (m *mask) add(p path) {
	for _, e := range p {
		var next *mask
		if e.isIdx {
			if m.elems == nil {
				m.elems = map[int]*mask{}
			}
			if next = m.elems[e.index]; next == nil {
				next = &mask{}
				m.elems[e.index] = next
			}
		} else {
			if m.fields == nil {
				m.fields = map[string]*mask{}
			}
			if next = m.fields[e.name]; next == nil {
				next = &mask{}
				m.fields[e.name] = next
			}
		}
		m = next
	}
	m.all = true
}

func (m *mask) apply(v *AV) *AV {
	switch {
	case v == nil:
		return nil
	case m.all:
		return v
	case m.fields != nil && v.M != nil:
		out := &AV{M: map[string]*AV{}}
		for name, sub := range m.fields {
			if x := sub.apply(v.M[name]); x != nil {
				out.M[name] = x
			}
		}
		if len(out.M) > 0 {
			return out
		}
	case m.elems != nil && v.L != nil:
		idx := make([]int, 0, len(m.elems))
		for i := range m.elems {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		out := &AV{L: []*AV{}}
		for _, i := range idx {
			if i < len(v.L) {
				if x := m.elems[i].apply(v.L[i]); x != nil {
					out.L = append(out.L, x)
				}
			}
		}
		if len(out.L) > 0 {
			return out
		}
	}
	return nil
}

// Updates.

func errUpdatePath() error {
	return validation("The document path provided in the update expression is invalid for update")
}

func errOperandType() error {
	return validation("An operand in the update expression has an incorrect data type")
}

// evalUpdateOperand evaluates a SET value against the original item.
func evalUpdateOperand(item Item, o operand) (*AV, error) {
	switch o := o.(type) {
	case ifNotExistsOperand:
		if v := resolve(item, o.p); v != nil {
			return v, nil
		}
		return evalUpdateOperand(item, o.v)
	case listAppendOperand:
		a, err := evalUpdateOperand(item, o.a)
		if err != nil {
			return nil, err
		}
		b, err := evalUpdateOperand(item, o.b)
		if err != nil {
			return nil, err
		}
		if a == nil || b == nil {
			return nil, validation("The provided expression refers to an attribute that does not exist in the item")
		}
		if a.L == nil || b.L == nil {
			return nil, validation("Invalid UpdateExpression: Incorrect operand type for operator or function; operator or function: list_append, operand type: %s", nonList(a, b))
		}
		return &AV{L: append(append([]*AV{}, a.L...), b.L...)}, nil
	case arithOperand:
		l, err := evalUpdateOperand(item, o.l)
		if err != nil {
			return nil, err
		}
		r, err := evalUpdateOperand(item, o.r)
		if err != nil {
			return nil, err
		}
		if l == nil || r == nil {
			return nil, validation("The provided expression refers to an attribute that does not exist in the item")
		}
		if l.N == nil || r.N == nil {
			return nil, errOperandType()
		}
		sign := 1
		if o.op == '-' {
			sign = -1
		}
		n, err := addNumbers(*l.N, *r.N, sign)
		if err != nil {
			return nil, err
		}
		return num(n), nil
	case pathOperand:
		v := resolve(item, o.p)
		if v == nil {
			return nil, validation("The provided expression refers to an attribute that does not exist in the item")
		}
		return v, nil
	case valueOperand:
		return o.v, nil
	}
	return nil, validation("Invalid UpdateExpression: unsupported operand")
}

func nonList(a, b *AV) string {
	if a.L == nil {
		return a.Type()
	}
	return b.Type()
}

// applyUpdate returns a new item with the actions applied. Values are
// evaluated against the original item first, as DynamoDB does.
func applyUpdate(orig Item, actions []updateAction) (Item, error) {
	values := make([]*AV, len(actions))
	for i, a := range actions {
		if a.kind == "SET" {
			v, err := evalUpdateOperand(orig, a.v)
			if err != nil {
				return nil, err
			}
			values[i] = v
		} else if a.v != nil {
			values[i] = evalOperand(orig, a.v)
		}
	}
	item := cloneItem(orig)
	if item == nil {
		item = Item{}
	}
	// List element removals run last and from the highest index down, so
	// every index refers to the original list.
	var removals []updateAction
	for i, a := range actions {
		var err error
		switch a.kind {
		case "SET":
			err = setPath(item, a.p, clone(values[i]))
		case "REMOVE":
			if a.p[len(a.p)-1].isIdx {
				removals = append(removals, a)
				continue
			}
			err = removePath(item, a.p)
		case "ADD":
			err = addPath(item, a.p, values[i])
		case "DELETE":
			err = deletePath(item, a.p, values[i])
		}
		if err != nil {
			return nil, err
		}
	}
	for i := 1; i < len(removals); i++ {
		for j := i; j > 0 && removals[j].p[len(removals[j].p)-1].index > removals[j-1].p[len(removals[j-1].p)-1].index; j-- {
			removals[j], removals[j-1] = removals[j-1], removals[j]
		}
	}
	for _, a := range removals {
		if err := removePath(item, a.p); err != nil {
			return nil, err
		}
	}
	return item, nil
}

// parent returns the container holding the last element of p.
func parent(item Item, p path) (*AV, error) {
	if len(p) == 1 {
		return nil, nil
	}
	v := resolve(item, p[:len(p)-1])
	last := p[len(p)-1]
	if v == nil || (last.isIdx && v.L == nil) || (!last.isIdx && v.M == nil) {
		return nil, errUpdatePath()
	}
	return v, nil
}

func setPath(item Item, p path, v *AV) error {
	if len(p) == 1 {
		item[p[0].name] = v
		return nil
	}
	c, err := parent(item, p)
	if err != nil {
		return err
	}
	last := p[len(p)-1]
	if !last.isIdx {
		c.M[last.name] = v
	} else if last.index < len(c.L) {
		c.L[last.index] = v
	} else {
		c.L = append(c.L, v)
	}
	return nil
}

func removePath(item Item, p path) error {
	if len(p) == 1 {
		delete(item, p[0].name)
		return nil
	}
	c := resolve(item, p[:len(p)-1])
	last := p[len(p)-1]
	switch {
	case c == nil:
		return nil
	case last.isIdx && c.L != nil:
		if last.index < len(c.L) {
			c.L = append(c.L[:last.index], c.L[last.index+1:]...)
		}
	case !last.isIdx && c.M != nil:
		delete(c.M, last.name)
	default:
		return errUpdatePath()
	}
	return nil
}

func addPath(item Item, p path, v *AV) error {
	cur := resolve(item, p)
	switch v.Type() {
	case "N":
		if cur == nil {
			return setPath(item, p, clone(v))
		}
		if cur.N == nil {
			return errOperandType()
		}
		n, err := addNumbers(*cur.N, *v.N, 1)
		if err != nil {
			return err
		}
		return setPath(item, p, num(n))
	case "SS", "NS", "BS":
		if cur == nil {
			return setPath(item, p, clone(v))
		}
		if cur.Type() != v.Type() {
			return errOperandType()
		}
		return setPath(item, p, setUnion(cur, v))
	}
	return validation("Invalid UpdateExpression: Incorrect operand type for operator or function; operator: ADD, operand type: %s", typeName(v))
}

func deletePath(item Item, p path, v *AV) error {
	switch v.Type() {
	case "SS", "NS", "BS":
	default:
		return validation("Invalid UpdateExpression: Incorrect operand type for operator or function; operator: DELETE, operand type: %s", typeName(v))
	}
	cur := resolve(item, p)
	if cur == nil {
		return nil
	}
	if cur.Type() != v.Type() {
		return errOperandType()
	}
	if rest := setDifference(cur, v); rest != nil {
		return setPath(item, p, rest)
	}
	return removePath(item, p)
}

func typeName(v *AV) string {
	switch v.Type() {
	case "S":
		return "STRING"
	case "N":
		return "NUMBER"
	case "B":
		return "BINARY"
	case "BOOL":
		return "BOOLEAN"
	case "L":
		return "LIST"
	case "M":
		return "MAP"
	}
	return v.Type()
}

// updatedPaths are the paths an update touched, for UPDATED_OLD/UPDATED_NEW.
func updatedPaths(actions []updateAction) []path {
	out := make([]path, len(actions))
	for i, a := range actions {
		out[i] = a.p
	}
	return out
}
