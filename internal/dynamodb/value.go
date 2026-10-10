package dynamodb

import (
	"bytes"
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// AV is a DynamoDB AttributeValue. Exactly one field is set.
type AV struct {
	S    *string
	N    *string
	B    []byte
	BOOL *bool
	NULL *bool
	SS   []string
	NS   []string
	BS   [][]byte
	L    []*AV
	M    map[string]*AV
}

// Item is a set of top-level attributes.
type Item map[string]*AV

// Type returns the AttributeValue type name, or "" if none is set.
func (v *AV) Type() string {
	switch {
	case v == nil:
		return ""
	case v.S != nil:
		return "S"
	case v.N != nil:
		return "N"
	case v.B != nil:
		return "B"
	case v.BOOL != nil:
		return "BOOL"
	case v.NULL != nil:
		return "NULL"
	case v.SS != nil:
		return "SS"
	case v.NS != nil:
		return "NS"
	case v.BS != nil:
		return "BS"
	case v.L != nil:
		return "L"
	case v.M != nil:
		return "M"
	}
	return ""
}

func (v *AV) setCount() int {
	n := 0
	for _, set := range []bool{v.S != nil, v.N != nil, v.B != nil, v.BOOL != nil, v.NULL != nil,
		v.SS != nil, v.NS != nil, v.BS != nil, v.L != nil, v.M != nil} {
		if set {
			n++
		}
	}
	return n
}

// MarshalJSON writes the single set member; empty lists and maps are kept.
func (v *AV) MarshalJSON() ([]byte, error) {
	var val interface{}
	switch v.Type() {
	case "S":
		val = *v.S
	case "N":
		val = *v.N
	case "B":
		val = v.B
	case "BOOL":
		val = *v.BOOL
	case "NULL":
		val = *v.NULL
	case "SS":
		val = v.SS
	case "NS":
		val = v.NS
	case "BS":
		val = v.BS
	case "L":
		val = v.L
	case "M":
		val = v.M
	default:
		return []byte("{}"), nil
	}
	b, err := json.Marshal(val)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(`{"`+v.Type()+`":`), b...), '}'), nil
}

func num(s string) *AV { return &AV{N: &s} }

// Validation and normalization.

var numberSyntax = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// normNumber returns the canonical form DynamoDB stores and returns: no
// exponent, no leading or trailing zeros, "-0" as "0".
func normNumber(s string) (string, error) {
	t := strings.TrimSpace(s)
	if !numberSyntax.MatchString(t) {
		return "", validation("The parameter cannot be converted to a numeric value: %s", s)
	}
	neg := strings.HasPrefix(t, "-")
	t = strings.TrimLeft(t, "+-")
	exp := 0
	if i := strings.IndexAny(t, "eE"); i >= 0 {
		e, err := strconv.Atoi(t[i+1:])
		if err != nil || e > 1000 || e < -1000 {
			return "", validation("Number overflow. Attempting to store a number with magnitude larger than supported range")
		}
		exp, t = e, t[:i]
	}
	intPart, frac, _ := strings.Cut(t, ".")
	digits := intPart + frac
	point := len(intPart) + exp // digits before the decimal point
	trimmed := strings.TrimLeft(digits, "0")
	point -= len(digits) - len(trimmed)
	digits = strings.TrimRight(trimmed, "0")
	if digits == "" {
		return "0", nil
	}
	if len(digits) > 38 {
		return "", validation("Attempting to store more than 38 significant digits in a Number")
	}
	if point-1 > 125 {
		return "", validation("Number overflow. Attempting to store a number with magnitude larger than supported range")
	}
	if point-1 < -130 {
		return "", validation("Number underflow. Attempting to store a number with magnitude smaller than supported range")
	}
	var out string
	switch {
	case point <= 0:
		out = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		out = digits + strings.Repeat("0", point-len(digits))
	default:
		out = digits[:point] + "." + digits[point:]
	}
	if neg {
		out = "-" + out
	}
	return out, nil
}

// normalize validates v recursively and canonicalizes numbers in place.
func normalize(v *AV) error {
	if v == nil {
		return validation("Supplied AttributeValue is empty, must contain exactly one of the supported datatypes")
	}
	switch n := v.setCount(); {
	case n == 0:
		return validation("Supplied AttributeValue is empty, must contain exactly one of the supported datatypes")
	case n > 1:
		return validation("Supplied AttributeValue has more than one datatypes set, must contain exactly one of the supported datatypes")
	}
	switch v.Type() {
	case "N":
		n, err := normNumber(*v.N)
		if err != nil {
			return err
		}
		v.N = &n
	case "NULL":
		if !*v.NULL {
			return validation("One or more parameter values were invalid: Null attribute value types must have the value of true")
		}
	case "SS", "NS", "BS":
		return normalizeSet(v)
	case "L":
		for _, e := range v.L {
			if err := normalize(e); err != nil {
				return err
			}
		}
	case "M":
		for _, e := range v.M {
			if err := normalize(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeSet(v *AV) error {
	t := v.Type()
	n := len(v.SS) + len(v.NS) + len(v.BS)
	if n == 0 {
		return validation("One or more parameter values were invalid: An %s attribute value must not be empty", t)
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		var k string
		switch t {
		case "SS":
			k = v.SS[i]
		case "NS":
			norm, err := normNumber(v.NS[i])
			if err != nil {
				return err
			}
			v.NS[i], k = norm, norm
		case "BS":
			k = string(v.BS[i])
		}
		if seen[k] {
			return validation("One or more parameter values were invalid: Input collection %s contains duplicates.", setString(v))
		}
		seen[k] = true
	}
	return nil
}

func setString(v *AV) string {
	switch v.Type() {
	case "SS":
		return "[" + strings.Join(v.SS, ", ") + "]"
	case "NS":
		return "[" + strings.Join(v.NS, ", ") + "]"
	}
	return "[...]"
}

func normalizeItem(item Item) error {
	for _, v := range item {
		if err := normalize(v); err != nil {
			return err
		}
	}
	return nil
}

// Comparison.

func rat(n string) *big.Rat {
	r, ok := new(big.Rat).SetString(n)
	if !ok {
		return new(big.Rat)
	}
	return r
}

// compare orders two scalars of the same type (S, N or B). ok is false if
// the types differ or aren't ordered.
func compare(a, b *AV) (c int, ok bool) {
	if a == nil || b == nil || a.Type() != b.Type() {
		return 0, false
	}
	switch a.Type() {
	case "S":
		return strings.Compare(*a.S, *b.S), true
	case "N":
		return rat(*a.N).Cmp(rat(*b.N)), true
	case "B":
		return bytes.Compare(a.B, b.B), true
	}
	return 0, false
}

// equal reports deep equality; sets compare as sets.
func equal(a, b *AV) bool {
	if a == nil || b == nil || a.Type() != b.Type() {
		return false
	}
	switch a.Type() {
	case "S", "N", "B":
		c, _ := compare(a, b)
		return c == 0
	case "BOOL":
		return *a.BOOL == *b.BOOL
	case "NULL":
		return true
	case "SS", "NS", "BS":
		ka, kb := setKeys(a), setKeys(b)
		if len(ka) != len(kb) {
			return false
		}
		for k := range ka {
			if !kb[k] {
				return false
			}
		}
		return true
	case "L":
		if len(a.L) != len(b.L) {
			return false
		}
		for i := range a.L {
			if !equal(a.L[i], b.L[i]) {
				return false
			}
		}
		return true
	case "M":
		if len(a.M) != len(b.M) {
			return false
		}
		for k, v := range a.M {
			if !equal(v, b.M[k]) {
				return false
			}
		}
		return true
	}
	return false
}

// setKeys returns the members of a set as comparable strings (numbers in
// canonical form).
func setKeys(v *AV) map[string]bool {
	out := map[string]bool{}
	for _, s := range v.SS {
		out[s] = true
	}
	for _, n := range v.NS {
		out[rat(n).RatString()] = true
	}
	for _, b := range v.BS {
		out[string(b)] = true
	}
	return out
}

// Arithmetic on numbers, for ADD and SET a = b + c.

func addNumbers(a, b string, sign int) (string, error) {
	r := rat(a)
	if sign < 0 {
		r.Sub(r, rat(b))
	} else {
		r.Add(r, rat(b))
	}
	return normNumber(ratDecimal(r))
}

// ratDecimal renders a rational with a terminating decimal expansion
// exactly. Every number parsed from decimal text has one.
func ratDecimal(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	d := new(big.Int).Set(r.Denom())
	twos, fives := 0, 0
	two, five, zero := big.NewInt(2), big.NewInt(5), new(big.Int)
	m := new(big.Int)
	for m.Mod(d, two).Cmp(zero) == 0 {
		d.Div(d, two)
		twos++
	}
	for m.Mod(d, five).Cmp(zero) == 0 {
		d.Div(d, five)
		fives++
	}
	prec := twos
	if fives > prec {
		prec = fives
	}
	return r.FloatString(prec)
}

// Sets.

func setUnion(a, b *AV) *AV {
	out := &AV{}
	seen := setKeys(a)
	switch a.Type() {
	case "SS":
		out.SS = append([]string(nil), a.SS...)
		for _, s := range b.SS {
			if !seen[s] {
				out.SS = append(out.SS, s)
				seen[s] = true
			}
		}
	case "NS":
		out.NS = append([]string(nil), a.NS...)
		for _, n := range b.NS {
			if k := rat(n).RatString(); !seen[k] {
				out.NS = append(out.NS, n)
				seen[k] = true
			}
		}
	case "BS":
		out.BS = append([][]byte(nil), a.BS...)
		for _, x := range b.BS {
			if !seen[string(x)] {
				out.BS = append(out.BS, x)
				seen[string(x)] = true
			}
		}
	}
	return out
}

// setDifference returns a minus b, or nil if nothing is left.
func setDifference(a, b *AV) *AV {
	drop := setKeys(b)
	out := &AV{}
	for _, s := range a.SS {
		if !drop[s] {
			out.SS = append(out.SS, s)
		}
	}
	for _, n := range a.NS {
		if !drop[rat(n).RatString()] {
			out.NS = append(out.NS, n)
		}
	}
	for _, x := range a.BS {
		if !drop[string(x)] {
			out.BS = append(out.BS, x)
		}
	}
	if out.SS == nil && out.NS == nil && out.BS == nil {
		return nil
	}
	return out
}

// Sizes, per the DynamoDB item size rules (approximately for numbers).

func avSize(v *AV) int {
	switch v.Type() {
	case "S":
		return len(*v.S)
	case "N":
		return numSize(*v.N)
	case "B":
		return len(v.B)
	case "BOOL", "NULL":
		return 1
	case "SS":
		n := 0
		for _, s := range v.SS {
			n += len(s)
		}
		return n
	case "NS":
		n := 0
		for _, s := range v.NS {
			n += numSize(s)
		}
		return n
	case "BS":
		n := 0
		for _, b := range v.BS {
			n += len(b)
		}
		return n
	case "L":
		n := 3
		for _, e := range v.L {
			n += 1 + avSize(e)
		}
		return n
	case "M":
		n := 3
		for k, e := range v.M {
			n += 1 + len(k) + avSize(e)
		}
		return n
	}
	return 0
}

func numSize(n string) int {
	digits := strings.Trim(strings.NewReplacer("-", "", ".", "").Replace(n), "0")
	return (len(digits)+1)/2 + 1
}

func itemSize(item Item) int {
	n := 0
	for k, v := range item {
		n += len(k) + avSize(v)
	}
	return n
}

// Copies. Stored items are never mutated in place, so readers may hold them
// after releasing the lock; updates work on a deep copy.

func clone(v *AV) *AV {
	if v == nil {
		return nil
	}
	c := *v
	switch v.Type() {
	case "L":
		c.L = make([]*AV, len(v.L))
		for i, e := range v.L {
			c.L[i] = clone(e)
		}
	case "M":
		c.M = make(map[string]*AV, len(v.M))
		for k, e := range v.M {
			c.M[k] = clone(e)
		}
	case "SS":
		c.SS = append([]string(nil), v.SS...)
	case "NS":
		c.NS = append([]string(nil), v.NS...)
	case "BS":
		c.BS = append([][]byte(nil), v.BS...)
	}
	return &c
}

func cloneItem(item Item) Item {
	if item == nil {
		return nil
	}
	out := make(Item, len(item))
	for k, v := range item {
		out[k] = clone(v)
	}
	return out
}

// keyString encodes a key attribute value for map lookups.
func keyString(v *AV) string {
	switch v.Type() {
	case "S":
		return "S" + *v.S
	case "N":
		return "N" + rat(*v.N).RatString()
	case "B":
		return "B" + string(v.B)
	}
	return ""
}
