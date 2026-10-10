// Package persist saves service state to disk and loads it back, so may can
// keep its resources across restarts (the -data-dir flag).
//
// Services keep their state in unexported structs, so the codec walks values
// with reflection, reading and writing unexported fields too. The format is
// JSON with Go field names, which tolerates fields being added or removed
// between versions (missing fields keep their zero value, unknown ones are
// ignored). Special forms:
//
//   - a pointer reached more than once is written as {"$id":n,"$v":value} the
//     first time and {"$ref":n} after that, so shared pointers stay shared;
//   - byte slices of BlobThreshold bytes or more are written to a blob store
//     as {"$blob":"<sha256>"}, smaller ones as base64 strings;
//   - non-nil interface values are {"$t":"<registered name>","$v":value};
//   - maps with non-string keys are arrays of [key, value] pairs, and string
//     keys starting with "$" get an extra "$";
//   - types implementing encoding.TextMarshaler (time.Time) use their text;
//   - pointer types registered with Intern are written as a name.
//
// Fields of kind chan, func and unsafe.Pointer, sync types, and fields tagged
// `persist:"-"` are skipped.
package persist

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"unicode/utf8"
	"unsafe"
)

// BlobThreshold is the size from which byte slices go to the blob store.
const BlobThreshold = 32 << 10

// Blobs stores large byte slices by content hash.
type Blobs interface {
	Put(data []byte) (string, error)
	Get(hash string) ([]byte, error)
}

var (
	regMu     sync.RWMutex
	byName    = map[string]reflect.Type{}
	nameOf    = map[reflect.Type]string{}
	interned  = map[reflect.Type]internFuncs{}
	textMarsh = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	textUnm   = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

type internFuncs struct {
	name   func(reflect.Value) string
	lookup func(string) (reflect.Value, bool)
}

func init() {
	Register("", false, int(0), int64(0), float64(0), json.Number(""), map[string]interface{}{}, []interface{}{})
}

// Register makes the dynamic types of values storable in interface fields.
func Register(values ...interface{}) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, v := range values {
		t := reflect.TypeOf(v)
		byName[typeName(t)] = t
		nameOf[t] = typeName(t)
	}
}

func typeName(t reflect.Type) string {
	if t.Name() != "" && t.PkgPath() != "" {
		return t.PkgPath() + "." + t.Name()
	}
	return t.String()
}

// Intern stores *T values as a name, for pointers into a fixed catalog that
// must stay identical to the catalog's entries after loading.
func Intern[T any](name func(*T) string, lookup func(string) *T) {
	regMu.Lock()
	defer regMu.Unlock()
	interned[reflect.TypeOf((*T)(nil))] = internFuncs{
		name: func(v reflect.Value) string { return name(v.Interface().(*T)) },
		lookup: func(s string) (reflect.Value, bool) {
			p := lookup(s)
			return reflect.ValueOf(p), p != nil
		},
	}
}

// writable returns v without the read-only flag that unexported fields
// carry, so it can be set and passed to Interface. v must be addressable.
func writable(v reflect.Value) reflect.Value {
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
}

// addressable returns v or an addressable copy of it.
func addressable(v reflect.Value) reflect.Value {
	if v.CanAddr() {
		return v
	}
	c := reflect.New(v.Type()).Elem()
	c.Set(v)
	return c
}

func skipField(f reflect.StructField) bool {
	if f.Tag.Get("persist") == "-" {
		return true
	}
	switch f.Type.Kind() {
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return true
	}
	return f.Type.PkgPath() == "sync" || f.Type.PkgPath() == "sync/atomic"
}

func usesText(t reflect.Type) bool {
	return t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface &&
		t.Implements(textMarsh) && reflect.PointerTo(t).Implements(textUnm)
}

type ptrKey struct {
	p uintptr
	t reflect.Type
}

// Encode writes v, a pointer, to w. Blobs may be nil if no byte slice
// reaches BlobThreshold.
func Encode(w io.Writer, v interface{}, blobs Blobs) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("persist: Encode needs a non-nil pointer, got %T", v)
	}
	e := &encoder{blobs: blobs, refs: map[ptrKey]int{}, ids: map[ptrKey]int{}}
	root := rv.Elem()
	e.count(root)
	bw := bufio.NewWriter(w)
	e.w = bw
	if err := e.value(root); err != nil {
		return err
	}
	bw.WriteByte('\n')
	return bw.Flush()
}

type encoder struct {
	w     *bufio.Writer
	blobs Blobs
	refs  map[ptrKey]int // times each pointer is reached
	ids   map[ptrKey]int // ids of shared pointers already written
	next  int
}

// count records how often each pointer is reached, to find shared ones.
func (e *encoder) count(v reflect.Value) {
	t := v.Type()
	if usesText(t) {
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		if _, ok := interned[t]; ok {
			return
		}
		k := ptrKey{v.Pointer(), t}
		e.refs[k]++
		if e.refs[k] == 1 {
			e.count(v.Elem())
		}
	case reflect.Interface:
		if !v.IsNil() {
			e.count(v.Elem())
		}
	case reflect.Struct:
		v = addressable(v)
		for i := 0; i < t.NumField(); i++ {
			if !skipField(t.Field(i)) {
				e.count(writable(v.Field(i)))
			}
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return
		}
		for i := 0; i < v.Len(); i++ {
			e.count(v.Index(i))
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			e.count(it.Key())
			e.count(it.Value())
		}
	}
}

func (e *encoder) str(s string) {
	b, _ := json.Marshal(s)
	e.w.Write(b)
}

func (e *encoder) value(v reflect.Value) error {
	t := v.Type()
	if usesText(t) {
		b, err := addressable(v).Interface().(encoding.TextMarshaler).MarshalText()
		if err != nil {
			return fmt.Errorf("persist: %s: %w", t, err)
		}
		e.str(string(b))
		return nil
	}
	switch t.Kind() {
	case reflect.Bool:
		e.w.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.w.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		e.w.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		switch {
		case math.IsNaN(f) || math.IsInf(f, 0):
			e.str(strconv.FormatFloat(f, 'g', -1, 64))
		default:
			e.w.WriteString(strconv.FormatFloat(f, 'g', -1, t.Bits()))
		}
	case reflect.String:
		s := v.String()
		if utf8.ValidString(s) {
			e.str(s)
		} else {
			e.w.WriteString(`{"$b64":`)
			e.str(base64.StdEncoding.EncodeToString([]byte(s)))
			e.w.WriteByte('}')
		}
	case reflect.Pointer:
		return e.pointer(v)
	case reflect.Interface:
		if v.IsNil() {
			e.w.WriteString("null")
			return nil
		}
		dyn := v.Elem()
		regMu.RLock()
		name, ok := nameOf[dyn.Type()]
		regMu.RUnlock()
		if !ok {
			return fmt.Errorf("persist: interface value of unregistered type %s", dyn.Type())
		}
		e.w.WriteString(`{"$t":`)
		e.str(name)
		e.w.WriteString(`,"$v":`)
		if err := e.value(dyn); err != nil {
			return err
		}
		e.w.WriteByte('}')
	case reflect.Struct:
		v = addressable(v)
		e.w.WriteByte('{')
		first := true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if skipField(f) {
				continue
			}
			if !first {
				e.w.WriteByte(',')
			}
			first = false
			e.str(f.Name)
			e.w.WriteByte(':')
			if err := e.value(writable(v.Field(i))); err != nil {
				return fmt.Errorf("%s.%w", f.Name, err)
			}
		}
		e.w.WriteByte('}')
	case reflect.Slice:
		if v.IsNil() {
			e.w.WriteString("null")
			return nil
		}
		if t.Elem().Kind() == reflect.Uint8 {
			return e.bytes(v.Bytes())
		}
		return e.array(v)
	case reflect.Array:
		return e.array(v)
	case reflect.Map:
		return e.mapValue(v)
	default:
		return fmt.Errorf("persist: unsupported type %s", t)
	}
	return nil
}

func (e *encoder) pointer(v reflect.Value) error {
	t := v.Type()
	if v.IsNil() {
		e.w.WriteString("null")
		return nil
	}
	if in, ok := interned[t]; ok {
		e.str(in.name(v))
		return nil
	}
	k := ptrKey{v.Pointer(), t}
	if e.refs[k] < 2 {
		return e.value(v.Elem())
	}
	if id, ok := e.ids[k]; ok {
		fmt.Fprintf(e.w, `{"$ref":%d}`, id)
		return nil
	}
	e.next++
	e.ids[k] = e.next
	fmt.Fprintf(e.w, `{"$id":%d,"$v":`, e.next)
	if err := e.value(v.Elem()); err != nil {
		return err
	}
	e.w.WriteByte('}')
	return nil
}

func (e *encoder) bytes(b []byte) error {
	if len(b) >= BlobThreshold && e.blobs != nil {
		hash, err := e.blobs.Put(b)
		if err != nil {
			return err
		}
		e.w.WriteString(`{"$blob":`)
		e.str(hash)
		e.w.WriteByte('}')
		return nil
	}
	e.str(base64.StdEncoding.EncodeToString(b))
	return nil
}

func (e *encoder) array(v reflect.Value) error {
	if v.Type().Elem().Kind() == reflect.Uint8 && v.Kind() == reflect.Array {
		b := make([]byte, v.Len())
		reflect.Copy(reflect.ValueOf(b), v)
		return e.bytes(b)
	}
	e.w.WriteByte('[')
	for i := 0; i < v.Len(); i++ {
		if i > 0 {
			e.w.WriteByte(',')
		}
		if err := e.value(v.Index(i)); err != nil {
			return fmt.Errorf("[%d].%w", i, err)
		}
	}
	e.w.WriteByte(']')
	return nil
}

func (e *encoder) mapValue(v reflect.Value) error {
	if v.IsNil() {
		e.w.WriteString("null")
		return nil
	}
	t := v.Type()
	if t.Key().Kind() == reflect.String {
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		e.w.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				e.w.WriteByte(',')
			}
			s := k.String()
			if len(s) > 0 && s[0] == '$' {
				s = "$" + s
			}
			e.str(s)
			e.w.WriteByte(':')
			if err := e.value(v.MapIndex(k)); err != nil {
				return fmt.Errorf("[%q].%w", k.String(), err)
			}
		}
		e.w.WriteByte('}')
		return nil
	}
	// Other keys: [[k, v], ...], sorted by the encoded key.
	type pair struct{ k, v []byte }
	var pairs []pair
	it := v.MapRange()
	for it.Next() {
		var kb, vb bytes.Buffer
		sub := &encoder{w: bufio.NewWriter(&kb), blobs: e.blobs, refs: e.refs, ids: e.ids, next: e.next}
		if err := sub.value(it.Key()); err != nil {
			return err
		}
		sub.w.Flush()
		sub.w = bufio.NewWriter(&vb)
		if err := sub.value(it.Value()); err != nil {
			return err
		}
		sub.w.Flush()
		e.next = sub.next
		pairs = append(pairs, pair{kb.Bytes(), vb.Bytes()})
	}
	sort.Slice(pairs, func(i, j int) bool { return bytes.Compare(pairs[i].k, pairs[j].k) < 0 })
	e.w.WriteByte('[')
	for i, p := range pairs {
		if i > 0 {
			e.w.WriteByte(',')
		}
		e.w.WriteByte('[')
		e.w.Write(p.k)
		e.w.WriteByte(',')
		e.w.Write(p.v)
		e.w.WriteByte(']')
	}
	e.w.WriteByte(']')
	return nil
}

// Decode reads what Encode wrote into v, a pointer. Maps and slices in v
// are replaced, not merged.
func Decode(r io.Reader, v interface{}, blobs Blobs) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("persist: Decode needs a non-nil pointer, got %T", v)
	}
	jd := json.NewDecoder(r)
	jd.UseNumber()
	var tree interface{}
	if err := jd.Decode(&tree); err != nil {
		return fmt.Errorf("persist: %w", err)
	}
	d := &decoder{blobs: blobs, ptrs: map[int]reflect.Value{}}
	return d.value(rv.Elem(), tree)
}

type decoder struct {
	blobs Blobs
	ptrs  map[int]reflect.Value // shared pointers by id
}

func typeErr(t reflect.Type, j interface{}) error {
	return fmt.Errorf("persist: cannot load %T into %s", j, t)
}

// value decodes j into v, which is addressable and writable.
func (d *decoder) value(v reflect.Value, j interface{}) error {
	t := v.Type()
	if usesText(t) {
		s, ok := j.(string)
		if !ok {
			return typeErr(t, j)
		}
		return v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s))
	}
	if j == nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
			v.Set(reflect.Zero(t))
			return nil
		}
		return typeErr(t, j)
	}
	switch t.Kind() {
	case reflect.Bool:
		b, ok := j.(bool)
		if !ok {
			return typeErr(t, j)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := j.(json.Number)
		if !ok {
			return typeErr(t, j)
		}
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil {
			return err
		}
		v.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, ok := j.(json.Number)
		if !ok {
			return typeErr(t, j)
		}
		u, err := strconv.ParseUint(string(n), 10, 64)
		if err != nil {
			return err
		}
		v.SetUint(u)
	case reflect.Float32, reflect.Float64:
		var s string
		switch x := j.(type) {
		case json.Number:
			s = string(x)
		case string:
			s = x
		default:
			return typeErr(t, j)
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		v.SetFloat(f)
	case reflect.String:
		switch x := j.(type) {
		case string:
			v.SetString(x)
		case map[string]interface{}:
			s, _ := x["$b64"].(string)
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return err
			}
			v.SetString(string(b))
		default:
			return typeErr(t, j)
		}
	case reflect.Pointer:
		return d.pointer(v, j)
	case reflect.Interface:
		m, ok := j.(map[string]interface{})
		name, _ := m["$t"].(string)
		if !ok || name == "" {
			return typeErr(t, j)
		}
		regMu.RLock()
		dt, ok := byName[name]
		regMu.RUnlock()
		if !ok {
			return fmt.Errorf("persist: unknown type %q", name)
		}
		dv := reflect.New(dt).Elem()
		if err := d.value(dv, m["$v"]); err != nil {
			return err
		}
		if !dt.AssignableTo(t) {
			return fmt.Errorf("persist: %s does not implement %s", dt, t)
		}
		v.Set(dv)
	case reflect.Struct:
		m, ok := j.(map[string]interface{})
		if !ok {
			return typeErr(t, j)
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if skipField(f) {
				continue
			}
			fj, ok := m[f.Name]
			if !ok {
				continue
			}
			if err := d.value(writable(v.Field(i)), fj); err != nil {
				return fmt.Errorf("%s.%w", f.Name, err)
			}
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			b, err := d.bytes(j)
			if err != nil {
				return err
			}
			v.SetBytes(b)
			return nil
		}
		a, ok := j.([]interface{})
		if !ok {
			return typeErr(t, j)
		}
		s := reflect.MakeSlice(t, len(a), len(a))
		for i, x := range a {
			if err := d.value(s.Index(i), x); err != nil {
				return fmt.Errorf("[%d].%w", i, err)
			}
		}
		v.Set(s)
	case reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			b, err := d.bytes(j)
			if err != nil {
				return err
			}
			reflect.Copy(v, reflect.ValueOf(b))
			return nil
		}
		a, ok := j.([]interface{})
		if !ok || len(a) != t.Len() {
			return typeErr(t, j)
		}
		for i, x := range a {
			if err := d.value(v.Index(i), x); err != nil {
				return err
			}
		}
	case reflect.Map:
		return d.mapValue(v, j)
	default:
		return fmt.Errorf("persist: unsupported type %s", t)
	}
	return nil
}

func (d *decoder) pointer(v reflect.Value, j interface{}) error {
	t := v.Type()
	if in, ok := interned[t]; ok {
		s, _ := j.(string)
		p, ok := in.lookup(s)
		if !ok {
			return fmt.Errorf("persist: unknown %s %q", t.Elem(), s)
		}
		v.Set(p)
		return nil
	}
	if m, ok := j.(map[string]interface{}); ok {
		if ref, ok := m["$ref"].(json.Number); ok {
			id, _ := ref.Int64()
			v.Set(d.shared(int(id), t))
			return nil
		}
		if idn, ok := m["$id"].(json.Number); ok {
			id, _ := idn.Int64()
			p := d.shared(int(id), t)
			v.Set(p)
			return d.value(p.Elem(), m["$v"])
		}
	}
	if !v.IsNil() {
		// A root like &struct{Roles *map[string]*role}{&s.roles} loads
		// through its pointers into the service's fields.
		return d.value(v.Elem(), j)
	}
	p := reflect.New(t.Elem())
	if err := d.value(p.Elem(), j); err != nil {
		return err
	}
	v.Set(p)
	return nil
}

// shared returns the pointer with the given id, allocating it the first
// time it is seen (a $ref may come before its $id when maps are involved).
func (d *decoder) shared(id int, t reflect.Type) reflect.Value {
	if p, ok := d.ptrs[id]; ok && p.Type() == t {
		return p
	}
	p := reflect.New(t.Elem())
	d.ptrs[id] = p
	return p
}

func (d *decoder) bytes(j interface{}) ([]byte, error) {
	switch x := j.(type) {
	case string:
		return base64.StdEncoding.DecodeString(x)
	case map[string]interface{}:
		hash, _ := x["$blob"].(string)
		if d.blobs == nil {
			return nil, fmt.Errorf("persist: blob %s but no blob store", hash)
		}
		return d.blobs.Get(hash)
	}
	return nil, typeErr(reflect.TypeOf([]byte(nil)), j)
}

func (d *decoder) mapValue(v reflect.Value, j interface{}) error {
	t := v.Type()
	m := reflect.MakeMap(t)
	if t.Key().Kind() == reflect.String {
		o, ok := j.(map[string]interface{})
		if !ok {
			return typeErr(t, j)
		}
		for k, x := range o {
			if len(k) > 1 && k[0] == '$' && k[1] == '$' {
				k = k[1:]
			}
			kv := reflect.New(t.Key()).Elem()
			kv.SetString(k)
			ev := reflect.New(t.Elem()).Elem()
			if err := d.value(ev, x); err != nil {
				return fmt.Errorf("[%q].%w", k, err)
			}
			m.SetMapIndex(kv, ev)
		}
	} else {
		a, ok := j.([]interface{})
		if !ok {
			return typeErr(t, j)
		}
		for _, x := range a {
			pair, ok := x.([]interface{})
			if !ok || len(pair) != 2 {
				return typeErr(t, x)
			}
			kv := reflect.New(t.Key()).Elem()
			if err := d.value(kv, pair[0]); err != nil {
				return err
			}
			ev := reflect.New(t.Elem()).Elem()
			if err := d.value(ev, pair[1]); err != nil {
				return err
			}
			m.SetMapIndex(kv, ev)
		}
	}
	v.Set(m)
	return nil
}

// Hash returns the blob name of data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
