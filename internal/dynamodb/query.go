package dynamodb

import (
	"hash/fnv"
	"net/http"
	"sort"
)

type readParams struct {
	TableName                 string
	IndexName                 string
	Select                    string
	Limit                     *int
	ConsistentRead            bool
	ExclusiveStartKey         Item
	ReturnConsumedCapacity    string
	ProjectionExpression      string
	FilterExpression          string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]*AV
	AttributesToGet           []string
	ConditionalOperator       string
}

type queryInput struct {
	readParams
	KeyConditionExpression string
	KeyConditions          map[string]legacyCondition
	QueryFilter            map[string]legacyCondition
	ScanIndexForward       *bool
}

type scanInput struct {
	readParams
	ScanFilter    map[string]legacyCondition
	Segment       *int
	TotalSegments *int
}

type readOutput struct {
	Items            *[]Item `json:",omitempty"` // nil only for Select=COUNT
	Count            int
	ScannedCount     int
	LastEvaluatedKey Item      `json:",omitempty"`
	ConsumedCapacity *consumed `json:",omitempty"`
}

func (s *Service) registerQueries() {
	handle(s, "Query", s.query)
	handle(s, "Scan", s.scan)
}

// read is a parsed Query or Scan.
type read struct {
	t      *table
	ix     *index // nil for the base table
	key    keySchema
	filter cond
	paths  []path
	sel    string
}

// prepare parses the parameters shared by Query and Scan. legacyFilter is
// QueryFilter or ScanFilter.
func (s *Service) prepareRead(in *readParams, ctx *exprContext, legacyFilter map[string]legacyCondition, legacyName string) (*read, error) {
	if in.Limit != nil && *in.Limit < 1 {
		return nil, validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1", *in.Limit)
	}
	rd := &read{}
	var err error
	if in.FilterExpression != "" && len(legacyFilter) > 0 {
		return nil, mixedParams(legacyName, "FilterExpression")
	}
	if len(legacyFilter) > 0 {
		if rd.filter, err = filterCond(legacyFilter, in.ConditionalOperator); err != nil {
			return nil, err
		}
	} else if in.FilterExpression != "" {
		if rd.filter, err = ctx.parseCondition("FilterExpression", in.FilterExpression); err != nil {
			return nil, err
		}
		if err := validateCond(rd.filter, "FilterExpression"); err != nil {
			return nil, err
		}
	}
	if rd.paths, err = projection(ctx, in.ProjectionExpression, in.AttributesToGet); err != nil {
		return nil, err
	}
	rd.sel = in.Select
	switch rd.sel {
	case "":
		if rd.paths != nil {
			rd.sel = "SPECIFIC_ATTRIBUTES"
		} else if in.IndexName != "" {
			rd.sel = "ALL_PROJECTED_ATTRIBUTES"
		} else {
			rd.sel = "ALL_ATTRIBUTES"
		}
	case "SPECIFIC_ATTRIBUTES":
		if rd.paths == nil {
			return nil, validation("1 validation error detected: SPECIFIC_ATTRIBUTES requires ProjectionExpression or AttributesToGet")
		}
	case "ALL_ATTRIBUTES", "ALL_PROJECTED_ATTRIBUTES", "COUNT":
		if rd.paths != nil {
			return nil, validation("Cannot specify the AttributesToGet when choosing to get %s", rd.sel)
		}
		if rd.sel == "ALL_PROJECTED_ATTRIBUTES" && in.IndexName == "" {
			return nil, validation("ALL_PROJECTED_ATTRIBUTES can be used only when Querying using an IndexName")
		}
	default:
		return nil, validation("1 validation error detected: Value '%s' at 'select' failed to satisfy constraint: Member must satisfy enum value set: [SPECIFIC_ATTRIBUTES, COUNT, ALL_ATTRIBUTES, ALL_PROJECTED_ATTRIBUTES]", rd.sel)
	}
	return rd, nil
}

// bind resolves the table and index. Callers hold s.mu.
func (s *Service) bind(rd *read, in *readParams) error {
	t, err := s.table(in.TableName)
	if err != nil {
		return err
	}
	rd.t, rd.key = t, t.key
	if in.IndexName != "" {
		ix, err := t.index(in.IndexName)
		if err != nil {
			return err
		}
		rd.ix, rd.key = ix, ix.key
		if ix.global && in.ConsistentRead {
			return validation("Consistent reads are not supported on global secondary indexes")
		}
		if ix.global && rd.sel == "ALL_ATTRIBUTES" && ix.projection.ProjectionType != "ALL" {
			return validation("One or more parameter values were invalid: Select type ALL_ATTRIBUTES is not supported for global secondary index %s because its projection type is not ALL", ix.name)
		}
	}
	if in.ExclusiveStartKey != nil {
		if err := normalizeItem(in.ExclusiveStartKey); err != nil {
			return err
		}
		want := append(t.key.names(), rd.key.names()...)
		for _, n := range want {
			if v := in.ExclusiveStartKey[n]; v == nil || v.Type() != t.attrTypes[n] {
				return validation("The provided starting key is invalid: The provided key element does not match the schema")
			}
		}
	}
	return nil
}

// less orders items in the read's view.
func (rd *read) less(a, b Item) bool {
	if rd.ix != nil {
		return rd.t.indexLess(rd.ix, a, b)
	}
	return rd.t.tableLess(a, b)
}

// lastKey is the LastEvaluatedKey for an item: table key plus index key.
func (rd *read) lastKey(item Item) Item {
	k := rd.t.keyOf(item)
	for _, n := range rd.key.names() {
		k[n] = item[n]
	}
	return k
}

// output renders an item for the response.
func (rd *read) output(item Item) Item {
	if rd.ix != nil && (rd.ix.global || rd.sel == "ALL_PROJECTED_ATTRIBUTES") {
		item = rd.t.projectIndex(rd.ix, item)
	}
	if rd.paths != nil {
		item = project(item, rd.paths)
	}
	return item
}

// run evaluates candidates in order, applying the limit, the 1 MB page
// size, the filter and the projection.
func (rd *read) run(rows []Item, limit *int, consistent bool, capMode string) readOutput {
	out := readOutput{}
	items := []Item{}
	if rd.sel != "COUNT" {
		out.Items = &items
	}
	size := 0
	for i, item := range rows {
		if (limit != nil && out.ScannedCount == *limit) || size >= 1<<20 {
			out.LastEvaluatedKey = rd.lastKey(rows[i-1])
			break
		}
		out.ScannedCount++
		size += itemSize(item)
		if rd.filter != nil && !evalCond(item, rd.filter) {
			continue
		}
		out.Count++
		if rd.sel != "COUNT" {
			items = append(items, rd.output(item))
		}
	}
	out.ConsumedCapacity = capacity(rd.t, capMode, readUnits(size, consistent))
	return out
}

// after drops rows up to and including the start key (in view order).
func (rd *read) after(rows []Item, start Item, forward bool) []Item {
	if start == nil {
		return rows
	}
	i := sort.Search(len(rows), func(i int) bool {
		if forward {
			return rd.less(start, rows[i])
		}
		return rd.less(rows[i], start)
	})
	return rows[i:]
}

// Query.

// keyCondition is a parsed key condition: the hash value and an optional
// range condition.
type keyCondition struct {
	hash  *AV
	rng   cond
	terms []cond
}

func (s *Service) query(_ *http.Request, in *queryInput) (interface{}, error) {
	ctx, err := newExprContext(in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}
	var kc cond
	switch {
	case in.KeyConditionExpression != "" && len(in.KeyConditions) > 0:
		return nil, mixedParams("KeyConditions", "KeyConditionExpression")
	case len(in.KeyConditions) > 0:
		if kc, err = filterCond(in.KeyConditions, ""); err != nil {
			return nil, err
		}
	case in.KeyConditionExpression != "":
		if kc, err = ctx.parseCondition("KeyConditionExpression", in.KeyConditionExpression); err != nil {
			return nil, err
		}
	default:
		return nil, validation("Either the KeyConditions or KeyConditionExpression parameter must be specified in the request.")
	}
	rd, err := s.prepareRead(&in.readParams, ctx, in.QueryFilter, "QueryFilter")
	if err != nil {
		return nil, err
	}
	if err := ctx.checkUnused(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.bind(rd, &in.readParams); err != nil {
		return nil, err
	}
	key, err := rd.keyCondition(kc)
	if err != nil {
		return nil, err
	}
	if rd.filter != nil {
		if err := rd.checkFilterKeys(rd.filter); err != nil {
			return nil, err
		}
	}
	rows := rd.partition(key.hash)
	if key.rng != nil {
		var matched []Item
		for _, item := range rows {
			if evalCond(item, key.rng) {
				matched = append(matched, item)
			}
		}
		rows = matched
	}
	forward := in.ScanIndexForward == nil || *in.ScanIndexForward
	if !forward {
		rev := make([]Item, len(rows))
		for i, item := range rows {
			rev[len(rows)-1-i] = item
		}
		rows = rev
	}
	rows = rd.after(rows, in.ExclusiveStartKey, forward)
	return rd.run(rows, in.Limit, in.ConsistentRead, in.ReturnConsumedCapacity), nil
}

// partition returns the items with the given hash key, in range order.
func (rd *read) partition(hash *AV) []Item {
	hk := keyString(hash)
	if rd.ix == nil {
		if p := rd.t.parts[hk]; p != nil {
			return p.items
		}
		return nil
	}
	rows := rd.t.rows(rd.ix)
	lo := sort.Search(len(rows), func(i int) bool { return keyString(rows[i][rd.key.hash]) >= hk })
	hi := sort.Search(len(rows), func(i int) bool { return keyString(rows[i][rd.key.hash]) > hk })
	return rows[lo:hi]
}

// keyCondition validates a key condition against the key schema.
func (rd *read) keyCondition(c cond) (*keyCondition, error) {
	kc := &keyCondition{}
	var flatten func(c cond) error
	flatten = func(c cond) error {
		switch c := c.(type) {
		case andCond:
			if err := flatten(c.l); err != nil {
				return err
			}
			return flatten(c.r)
		case orCond:
			return validation("Invalid operator used in KeyConditionExpression: OR")
		case notCond:
			return validation("Invalid operator used in KeyConditionExpression: NOT")
		case inCond:
			return validation("Invalid operator used in KeyConditionExpression: IN")
		}
		kc.terms = append(kc.terms, c)
		return nil
	}
	if err := flatten(c); err != nil {
		return nil, err
	}
	if len(kc.terms) > 2 {
		return nil, validation("KeyConditionExpressions must only contain one condition per key")
	}
	for _, term := range kc.terms {
		name, value, err := keyTerm(term)
		if err != nil {
			return nil, err
		}
		switch name {
		case rd.key.hash:
			cc, ok := term.(compareCond)
			if !ok || cc.op != "=" {
				return nil, validation("Query key condition not supported")
			}
			if kc.hash != nil {
				return nil, validation("KeyConditionExpressions must only contain one condition per key")
			}
			kc.hash = value
		case rd.key.rng:
			if kc.rng != nil {
				return nil, validation("KeyConditionExpressions must only contain one condition per key")
			}
			if cc, ok := term.(compareCond); ok && cc.op == "<>" {
				return nil, validation("Unsupported operator on KeyConditionExpression: operator: <>")
			}
			kc.rng = term
		default:
			return nil, validation("Query condition missed key schema element: %s", rd.key.hash)
		}
		for _, v := range termValues(term) {
			if v.Type() != rd.t.attrTypes[name] {
				return nil, invalidParam("Condition parameter type does not match schema type")
			}
		}
	}
	if kc.hash == nil {
		return nil, validation("Query condition missed key schema element: %s", rd.key.hash)
	}
	return kc, nil
}

// keyTerm returns the key attribute a key-condition term constrains.
func keyTerm(c cond) (string, *AV, error) {
	var target operand
	var value *AV
	switch c := c.(type) {
	case compareCond:
		target = c.l
		if v, ok := c.r.(valueOperand); ok {
			value = v.v
		} else if v, ok := c.l.(valueOperand); ok {
			// ":v = pk" is accepted too.
			target, value = c.r, v.v
		}
	case betweenCond:
		target = c.x
	case funcCond:
		if c.name != "begins_with" {
			return "", nil, validation("Invalid operator used in KeyConditionExpression: %s", c.name)
		}
		target = c.args[0]
	default:
		return "", nil, validation("Query key condition not supported")
	}
	p, ok := target.(pathOperand)
	if !ok || len(p.p) != 1 {
		return "", nil, validation("Query key condition not supported")
	}
	return p.p[0].name, value, nil
}

func termValues(c cond) []*AV {
	var out []*AV
	add := func(o operand) {
		if v, ok := o.(valueOperand); ok {
			out = append(out, v.v)
		}
	}
	switch c := c.(type) {
	case compareCond:
		add(c.l)
		add(c.r)
	case betweenCond:
		add(c.lo)
		add(c.hi)
	case funcCond:
		add(c.args[1])
	}
	return out
}

// checkFilterKeys rejects filters on the key attributes being queried.
func (rd *read) checkFilterKeys(c cond) error {
	var err error
	walkPaths(c, func(p path) {
		for _, k := range rd.key.names() {
			if err == nil && p[0].name == k {
				err = validation("Filter Expression can only contain non-primary key attributes: Primary key attribute: %s", k)
			}
		}
	})
	return err
}

func walkPaths(c cond, fn func(path)) {
	visit := func(o operand) {
		switch o := o.(type) {
		case pathOperand:
			fn(o.p)
		case sizeOperand:
			fn(o.p)
		}
	}
	switch c := c.(type) {
	case andCond:
		walkPaths(c.l, fn)
		walkPaths(c.r, fn)
	case orCond:
		walkPaths(c.l, fn)
		walkPaths(c.r, fn)
	case notCond:
		walkPaths(c.c, fn)
	case compareCond:
		visit(c.l)
		visit(c.r)
	case betweenCond:
		visit(c.x)
		visit(c.lo)
		visit(c.hi)
	case inCond:
		visit(c.x)
		for _, o := range c.list {
			visit(o)
		}
	case funcCond:
		for _, o := range c.args {
			visit(o)
		}
	}
}

// Scan.

func (s *Service) scan(_ *http.Request, in *scanInput) (interface{}, error) {
	if (in.Segment == nil) != (in.TotalSegments == nil) {
		return nil, validation("The TotalSegments parameter is required but was not present in the request when Segment parameter is present")
	}
	if in.TotalSegments != nil {
		if *in.TotalSegments < 1 || *in.TotalSegments > 1000000 {
			return nil, validation("1 validation error detected: Value '%d' at 'totalSegments' failed to satisfy constraint: Member must have value less than or equal to 1000000", *in.TotalSegments)
		}
		if *in.Segment < 0 || *in.Segment >= *in.TotalSegments {
			return nil, validation("The Segment parameter is zero-based and must be less than parameter TotalSegments: Segment: %d is not less than TotalSegments: %d", *in.Segment, *in.TotalSegments)
		}
	}
	ctx, err := newExprContext(in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}
	rd, err := s.prepareRead(&in.readParams, ctx, in.ScanFilter, "ScanFilter")
	if err != nil {
		return nil, err
	}
	if err := ctx.checkUnused(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.bind(rd, &in.readParams); err != nil {
		return nil, err
	}
	var rows []Item
	if rd.ix != nil {
		rows = rd.t.rows(rd.ix)
	} else {
		rows = make([]Item, 0, rd.t.count)
		for _, p := range rd.t.partitions() {
			rows = append(rows, p.items...)
		}
	}
	if in.TotalSegments != nil && *in.TotalSegments > 1 {
		var seg []Item
		for _, item := range rows {
			h := fnv.New32a()
			h.Write([]byte(keyString(item[rd.key.hash])))
			if int(h.Sum32()%uint32(*in.TotalSegments)) == *in.Segment {
				seg = append(seg, item)
			}
		}
		rows = seg
	}
	rows = rd.after(rows, in.ExclusiveStartKey, true)
	return rd.run(rows, in.Limit, in.ConsistentRead, in.ReturnConsumedCapacity), nil
}
