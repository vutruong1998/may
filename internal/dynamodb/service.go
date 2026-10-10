// Package dynamodb implements the DynamoDB JSON API: tables with local and
// global secondary indexes, item reads and writes, Query and Scan, batches,
// transactions and the full expression language.
package dynamodb

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

// Service holds every table. Table names are unique across regions in may;
// each table records the region it was created in.
type Service struct {
	mu     sync.Mutex // also guards the lazily rebuilt index views
	region string
	tables map[string]*table
	tokens map[string]time.Time // transaction idempotency tokens
	api    *awsproto.JSONService
}

func New(region string) *Service {
	s := &Service{region: region}
	s.Reset()
	s.api = &awsproto.JSONService{
		Prefix:      "DynamoDB_20120810",
		Version:     "1.0",
		ErrorPrefix: "com.amazonaws.dynamodb.v20120810#",
		CRC32:       true,
		Handlers:    map[string]awsproto.JSONHandler{},
	}
	s.registerTables()
	s.registerItems()
	s.registerQueries()
	s.registerBatches()
	return s
}

// Reset deletes every table.
func (s *Service) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tables = map[string]*table{}
	s.tokens = map[string]time.Time{}
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.api.ServeHTTP(w, r) }

// handle registers an operation with a typed input.
func handle[In any](s *Service, op string, fn func(r *http.Request, in *In) (interface{}, error)) {
	s.api.Handlers[op] = awsproto.HandleJSON(fn)
}

// Errors.

func ddbError(code, format string, args ...interface{}) *awsproto.Error {
	return awsproto.Errorf(http.StatusBadRequest, code, format, args...)
}

func validation(format string, args ...interface{}) *awsproto.Error {
	return ddbError("ValidationException", format, args...)
}

func invalidParam(format string, args ...interface{}) *awsproto.Error {
	return validation("One or more parameter values were invalid: "+format, args...)
}

func errTableNotFound(name string) error {
	return ddbError("ResourceNotFoundException", "Requested resource not found: Table: %s not found", name)
}

func awsMessage(err error) string { return awsproto.AsError(err).Message }

func joinSorted(s []string) string {
	sort.Strings(s)
	return strings.Join(s, ", ")
}

// Tables and indexes.

type keySchema struct {
	hash, rng string // attribute names; rng is "" for hash-only keys
}

func (k keySchema) names() []string {
	if k.rng == "" {
		return []string{k.hash}
	}
	return []string{k.hash, k.rng}
}

type index struct {
	name       string
	global     bool
	key        keySchema
	projection Projection
	throughput *throughput
	created    time.Time

	// Sorted view of the items carrying this index's keys, rebuilt lazily
	// after writes.
	version int
	rows    []Item `persist:"-"`
}

type throughput struct {
	read, write int64
	increased   time.Time
	decreased   time.Time
}

type table struct {
	name, arn, id, region string
	created               time.Time
	key                   keySchema
	attrTypes             map[string]string // AttributeDefinitions
	billing               string
	billingChanged        time.Time
	throughput            *throughput
	indexes               []*index
	tags                  map[string]string
	stream                *StreamSpecification
	streamLabel           string
	sse                   *SSESpecification
	tableClass            string
	deletionProtection    bool
	ttl                   *TimeToLiveSpecification
	pitr                  bool

	// Items are grouped by partition key; each partition is sorted by the
	// range key. order caches the partitions sorted for Scan.
	parts   map[string]*partition
	order   []*partition `persist:"-"`
	count   int
	size    int
	version int
}

type partition struct {
	key   string // keyString of the hash value
	items []Item
}

func (s *Service) table(name string) (*table, error) {
	if name == "" {
		return nil, validation("1 validation error detected: Value null at 'tableName' failed to satisfy constraint: Member must not be null")
	}
	// Most operations also accept a table ARN.
	if strings.HasPrefix(name, "arn:") {
		if i := strings.Index(name, ":table/"); i >= 0 {
			name = name[i+len(":table/"):]
		}
	}
	t, ok := s.tables[name]
	if !ok {
		return nil, errTableNotFound(name)
	}
	return t, nil
}

func (t *table) index(name string) (*index, error) {
	for _, ix := range t.indexes {
		if ix.name == name {
			return ix, nil
		}
	}
	return nil, validation("The table does not have the specified index: %s", name)
}

// keyOf extracts the primary key attributes of an item.
func (t *table) keyOf(item Item) Item {
	k := Item{t.key.hash: item[t.key.hash]}
	if t.key.rng != "" {
		k[t.key.rng] = item[t.key.rng]
	}
	return k
}

// checkKey validates a Key parameter: exactly the key attributes, with the
// declared types.
func (t *table) checkKey(key Item) error {
	if err := normalizeItem(key); err != nil {
		return err
	}
	if len(key) != len(t.key.names()) {
		return invalidParam("The provided key element does not match the schema")
	}
	for _, n := range t.key.names() {
		v, ok := key[n]
		if !ok || v.Type() != t.attrTypes[n] {
			return invalidParam("The provided key element does not match the schema")
		}
		if (v.S != nil && *v.S == "") || (v.B != nil && len(v.B) == 0) {
			return invalidParam("The AttributeValue for a key attribute cannot contain an empty %s value. Key: %s", typeLong(v.Type()), n)
		}
	}
	return nil
}

// checkItem validates an item to be written: key attributes present and
// typed, index key attributes typed, and the size limit.
func (t *table) checkItem(item Item) error {
	if err := normalizeItem(item); err != nil {
		return err
	}
	for _, n := range t.key.names() {
		v, ok := item[n]
		if !ok {
			return invalidParam("Missing the key %s in the item", n)
		}
		if v.Type() != t.attrTypes[n] {
			return invalidParam("Type mismatch for key %s expected: %s actual: %s", n, t.attrTypes[n], v.Type())
		}
		if (v.S != nil && *v.S == "") || (v.B != nil && len(v.B) == 0) {
			return invalidParam("The AttributeValue for a key attribute cannot contain an empty %s value. Key: %s", typeLong(v.Type()), n)
		}
	}
	for _, ix := range t.indexes {
		for _, n := range ix.key.names() {
			v, ok := item[n]
			if !ok {
				continue
			}
			if v.Type() != t.attrTypes[n] {
				return invalidParam("Type mismatch for Index Key %s Expected: %s Actual: %s IndexName: %s", n, t.attrTypes[n], v.Type(), ix.name)
			}
			if (v.S != nil && *v.S == "") || (v.B != nil && len(v.B) == 0) {
				return invalidParam("One or more parameter values are not valid. The AttributeValue for a key attribute cannot contain an empty %s value. IndexName: %s, IndexKey: %s", typeLong(v.Type()), ix.name, n)
			}
		}
	}
	if itemSize(item) > 400*1024 {
		return validation("Item size has exceeded the maximum allowed size")
	}
	return nil
}

func typeLong(t string) string {
	switch t {
	case "S":
		return "string"
	case "B":
		return "binary"
	}
	return t
}

// get returns the stored item with the given (validated) key, or nil.
func (t *table) get(key Item) Item {
	p := t.parts[keyString(key[t.key.hash])]
	if p == nil {
		return nil
	}
	i, found := t.search(p, key)
	if !found {
		return nil
	}
	return p.items[i]
}

// search finds the position of key's range value within a partition.
func (t *table) search(p *partition, key Item) (int, bool) {
	if t.key.rng == "" {
		return 0, len(p.items) > 0
	}
	want := key[t.key.rng]
	i := sort.Search(len(p.items), func(i int) bool {
		c, _ := compare(p.items[i][t.key.rng], want)
		return c >= 0
	})
	if i < len(p.items) {
		c, _ := compare(p.items[i][t.key.rng], want)
		return i, c == 0
	}
	return i, false
}

// put stores an item, replacing any item with the same key.
func (t *table) put(item Item) {
	hk := keyString(item[t.key.hash])
	p := t.parts[hk]
	if p == nil {
		p = &partition{key: hk}
		t.parts[hk] = p
		t.order = nil
	}
	i, found := t.search(p, item)
	if found {
		t.size -= itemSize(p.items[i])
		p.items[i] = item
	} else {
		p.items = append(p.items, nil)
		copy(p.items[i+1:], p.items[i:])
		p.items[i] = item
		t.count++
	}
	t.size += itemSize(item)
	t.version++
}

// remove deletes the item with the given key, if present.
func (t *table) remove(key Item) {
	hk := keyString(key[t.key.hash])
	p := t.parts[hk]
	if p == nil {
		return
	}
	i, found := t.search(p, key)
	if !found {
		return
	}
	t.size -= itemSize(p.items[i])
	p.items = append(p.items[:i], p.items[i+1:]...)
	t.count--
	if len(p.items) == 0 {
		delete(t.parts, hk)
		t.order = nil
	}
	t.version++
}

// partitions returns partitions in scan order.
func (t *table) partitions() []*partition {
	if t.order == nil {
		t.order = make([]*partition, 0, len(t.parts))
		for _, p := range t.parts {
			t.order = append(t.order, p)
		}
		sort.Slice(t.order, func(i, j int) bool { return t.order[i].key < t.order[j].key })
	}
	return t.order
}

// rows returns the index's items in index order: by hash key, range key,
// then table key. Callers hold s.mu.
func (t *table) rows(ix *index) []Item {
	if ix.rows != nil && ix.version == t.version {
		return ix.rows
	}
	rows := []Item{}
	for _, p := range t.partitions() {
		for _, item := range p.items {
			if item[ix.key.hash] != nil && (ix.key.rng == "" || item[ix.key.rng] != nil) {
				rows = append(rows, item)
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return t.indexLess(ix, rows[i], rows[j]) })
	ix.rows, ix.version = rows, t.version
	return rows
}

// indexLess orders items by index key, then table key.
func (t *table) indexLess(ix *index, a, b Item) bool {
	if ka, kb := keyString(a[ix.key.hash]), keyString(b[ix.key.hash]); ka != kb {
		return ka < kb
	}
	if ix.key.rng != "" {
		if c, _ := compare(a[ix.key.rng], b[ix.key.rng]); c != 0 {
			return c < 0
		}
	}
	return t.tableLess(a, b)
}

func (t *table) tableLess(a, b Item) bool {
	if ka, kb := keyString(a[t.key.hash]), keyString(b[t.key.hash]); ka != kb {
		return ka < kb
	}
	if t.key.rng != "" {
		c, _ := compare(a[t.key.rng], b[t.key.rng])
		return c < 0
	}
	return false
}

// project applies an index projection to a table item.
func (t *table) projectIndex(ix *index, item Item) Item {
	if ix.projection.ProjectionType == "ALL" {
		return item
	}
	out := Item{}
	for _, n := range append(t.key.names(), ix.key.names()...) {
		if v, ok := item[n]; ok {
			out[n] = v
		}
	}
	if ix.projection.ProjectionType == "INCLUDE" {
		for _, n := range ix.projection.NonKeyAttributes {
			if v, ok := item[n]; ok {
				out[n] = v
			}
		}
	}
	return out
}

func nowFloat(t time.Time) float64 { return float64(t.UnixNano()/int64(time.Millisecond)) / 1000 }
