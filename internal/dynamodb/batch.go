package dynamodb

import (
	"net/http"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

type keysAndAttributes struct {
	Keys                     []Item
	ProjectionExpression     string
	ExpressionAttributeNames map[string]string
	AttributesToGet          []string
	ConsistentRead           bool
}

type batchGetInput struct {
	RequestItems           map[string]keysAndAttributes
	ReturnConsumedCapacity string
}

type writeRequest struct {
	PutRequest    *struct{ Item Item }
	DeleteRequest *struct{ Key Item }
}

type batchWriteInput struct {
	RequestItems                map[string][]writeRequest
	ReturnConsumedCapacity      string
	ReturnItemCollectionMetrics string
}

func (s *Service) registerBatches() {
	handle(s, "BatchGetItem", s.batchGet)
	handle(s, "BatchWriteItem", s.batchWrite)
	handle(s, "TransactGetItems", s.transactGet)
	handle(s, "TransactWriteItems", s.transactWrite)
}

// itemKey identifies an item across tables, to detect duplicates.
func itemKey(t *table, key Item) string {
	k := t.name + "\x00" + keyString(key[t.key.hash])
	if t.key.rng != "" {
		k += "\x00" + keyString(key[t.key.rng])
	}
	return k
}

func (s *Service) batchGet(_ *http.Request, in *batchGetInput) (interface{}, error) {
	if len(in.RequestItems) == 0 {
		return nil, validation("1 validation error detected: Value null at 'requestItems' failed to satisfy constraint: Member must have length greater than or equal to 1")
	}
	total := 0
	type plan struct {
		paths []path
		keys  []Item
	}
	plans := map[string]plan{}
	for name, ka := range in.RequestItems {
		total += len(ka.Keys)
		if len(ka.Keys) == 0 {
			return nil, validation("1 validation error detected: Value '[]' at 'requestItems.%s.member.keys' failed to satisfy constraint: Member must have length greater than or equal to 1", name)
		}
		ctx, err := newExprContext(ka.ExpressionAttributeNames, nil)
		if err != nil {
			return nil, err
		}
		paths, err := projection(ctx, ka.ProjectionExpression, ka.AttributesToGet)
		if err != nil {
			return nil, err
		}
		if err := ctx.checkUnused(); err != nil {
			return nil, err
		}
		plans[name] = plan{paths, ka.Keys}
	}
	if total > 100 {
		return nil, validation("Too many items requested for the BatchGetItem call")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := struct {
		Responses        map[string][]Item
		UnprocessedKeys  map[string]keysAndAttributes
		ConsumedCapacity []*consumed `json:",omitempty"`
	}{Responses: map[string][]Item{}, UnprocessedKeys: map[string]keysAndAttributes{}}
	for name, p := range plans {
		t, err := s.table(name)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		items := []Item{}
		size := 0
		for _, key := range p.keys {
			if err := t.checkKey(key); err != nil {
				return nil, err
			}
			k := itemKey(t, key)
			if seen[k] {
				return nil, validation("Provided list of item keys contains duplicates")
			}
			seen[k] = true
			if item := t.get(key); item != nil {
				size += itemSize(item)
				if p.paths != nil {
					item = project(item, p.paths)
				}
				items = append(items, item)
			}
		}
		out.Responses[t.name] = items
		if c := capacity(t, in.ReturnConsumedCapacity, readUnits(size, in.RequestItems[name].ConsistentRead)); c != nil {
			out.ConsumedCapacity = append(out.ConsumedCapacity, c)
		}
	}
	return out, nil
}

func (s *Service) batchWrite(_ *http.Request, in *batchWriteInput) (interface{}, error) {
	if len(in.RequestItems) == 0 {
		return nil, validation("1 validation error detected: Value null at 'requestItems' failed to satisfy constraint: Member must have length greater than or equal to 1")
	}
	total := 0
	for _, reqs := range in.RequestItems {
		total += len(reqs)
	}
	if total > 25 {
		return nil, validation("1 validation error detected: Value at 'requestItems' failed to satisfy constraint: Map value must satisfy constraint: [Member must have length less than or equal to 25, Member must have length greater than or equal to 1]")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Validate everything first: a batch with an invalid request writes nothing.
	type op struct {
		t    *table
		put  Item
		del  Item
		size float64
	}
	var ops []op
	seen := map[string]bool{}
	for name, reqs := range in.RequestItems {
		t, err := s.table(name)
		if err != nil {
			return nil, err
		}
		for _, r := range reqs {
			var o op
			switch {
			case r.PutRequest != nil && r.DeleteRequest == nil:
				if err := t.checkItem(r.PutRequest.Item); err != nil {
					return nil, err
				}
				o = op{t: t, put: r.PutRequest.Item}
			case r.DeleteRequest != nil && r.PutRequest == nil:
				if err := t.checkKey(r.DeleteRequest.Key); err != nil {
					return nil, err
				}
				o = op{t: t, del: r.DeleteRequest.Key}
			default:
				return nil, validation("Supplied AttributeValue has more than one datatypes set, must contain exactly one of the supported datatypes")
			}
			key := o.del
			if key == nil {
				key = t.keyOf(o.put)
			}
			k := itemKey(t, key)
			if seen[k] {
				return nil, validation("Provided list of item keys contains duplicates")
			}
			seen[k] = true
			ops = append(ops, o)
		}
	}
	units := map[*table]float64{}
	for _, o := range ops {
		if o.put != nil {
			units[o.t] += writeUnits(o.t.get(o.t.keyOf(o.put)), o.put)
			o.t.put(o.put)
		} else {
			units[o.t] += writeUnits(o.t.get(o.del))
			o.t.remove(o.del)
		}
	}
	out := struct {
		UnprocessedItems map[string][]writeRequest
		ConsumedCapacity []*consumed `json:",omitempty"`
	}{UnprocessedItems: map[string][]writeRequest{}}
	for t, u := range units {
		if c := capacity(t, in.ReturnConsumedCapacity, u); c != nil {
			out.ConsumedCapacity = append(out.ConsumedCapacity, c)
		}
	}
	return out, nil
}

// Transactions.

type transactGetInput struct {
	TransactItems []struct {
		Get *struct {
			TableName                string
			Key                      Item
			ProjectionExpression     string
			ExpressionAttributeNames map[string]string
		}
	}
	ReturnConsumedCapacity string
}

func (s *Service) transactGet(_ *http.Request, in *transactGetInput) (interface{}, error) {
	if len(in.TransactItems) == 0 || len(in.TransactItems) > 100 {
		return nil, validation("1 validation error detected: Value at 'transactItems' failed to satisfy constraint: Member must have length less than or equal to 100, Member must have length greater than or equal to 1")
	}
	paths := make([][]path, len(in.TransactItems))
	for i, ti := range in.TransactItems {
		if ti.Get == nil {
			return nil, validation("1 validation error detected: Value null at 'transactItems.%d.member.get' failed to satisfy constraint: Member must not be null", i+1)
		}
		ctx, err := newExprContext(ti.Get.ExpressionAttributeNames, nil)
		if err != nil {
			return nil, err
		}
		if paths[i], err = projection(ctx, ti.Get.ProjectionExpression, nil); err != nil {
			return nil, err
		}
		if err := ctx.checkUnused(); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	type response struct {
		Item Item `json:",omitempty"`
	}
	out := struct {
		Responses        []response
		ConsumedCapacity []*consumed `json:",omitempty"`
	}{Responses: []response{}}
	units := map[*table]float64{}
	seen := map[string]bool{}
	for i, ti := range in.TransactItems {
		t, err := s.table(ti.Get.TableName)
		if err != nil {
			return nil, err
		}
		if err := t.checkKey(ti.Get.Key); err != nil {
			return nil, err
		}
		k := itemKey(t, ti.Get.Key)
		if seen[k] {
			return nil, validation("Transaction request cannot include multiple operations on one item")
		}
		seen[k] = true
		item := t.get(ti.Get.Key)
		units[t] += 2 * readUnits(itemSize(item), true)
		if item != nil && paths[i] != nil {
			item = project(item, paths[i])
		}
		out.Responses = append(out.Responses, response{item})
	}
	for t, u := range units {
		if c := capacity(t, in.ReturnConsumedCapacity, u); c != nil {
			out.ConsumedCapacity = append(out.ConsumedCapacity, c)
		}
	}
	return out, nil
}

type transactWriteItem struct {
	ConditionCheck *struct {
		TableName string
		Key       Item
		exprParams
		ReturnValuesOnConditionCheckFailure string
	}
	Put *struct {
		TableName string
		Item      Item
		exprParams
		ReturnValuesOnConditionCheckFailure string
	}
	Delete *struct {
		TableName string
		Key       Item
		exprParams
		ReturnValuesOnConditionCheckFailure string
	}
	Update *struct {
		TableName        string
		Key              Item
		UpdateExpression string
		exprParams
		ReturnValuesOnConditionCheckFailure string
	}
}

type transactWriteInput struct {
	TransactItems               []transactWriteItem
	ClientRequestToken          string
	ReturnConsumedCapacity      string
	ReturnItemCollectionMetrics string
}

type cancellationReason struct {
	Item    Item `json:",omitempty"`
	Code    string
	Message string `json:",omitempty"`
}

// pendingWrite is one validated transactional operation.
type pendingWrite struct {
	t       *table
	key     Item
	cond    cond
	rv      string
	kind    string // ConditionCheck, Put, Delete, Update
	item    Item   // Put item
	actions []updateAction
}

func (s *Service) transactWrite(_ *http.Request, in *transactWriteInput) (interface{}, error) {
	if len(in.TransactItems) == 0 || len(in.TransactItems) > 100 {
		return nil, validation("1 validation error detected: Value at 'transactItems' failed to satisfy constraint: Member must have length less than or equal to 100, Member must have length greater than or equal to 1")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for tok, exp := range s.tokens {
		if now.After(exp) {
			delete(s.tokens, tok)
		}
	}
	if in.ClientRequestToken != "" {
		if _, ok := s.tokens[in.ClientRequestToken]; ok {
			return struct{}{}, nil // idempotent replay
		}
	}

	ops := make([]pendingWrite, len(in.TransactItems))
	seen := map[string]bool{}
	for i, ti := range in.TransactItems {
		op, err := s.prepareWrite(ti)
		if err != nil {
			return nil, err
		}
		k := itemKey(op.t, op.key)
		if seen[k] {
			return nil, validation("Transaction request cannot include multiple operations on one item")
		}
		seen[k] = true
		ops[i] = op
	}

	// Evaluate every condition before writing anything.
	reasons := make([]cancellationReason, len(ops))
	results := make([]Item, len(ops))
	failed := false
	for i, op := range ops {
		reasons[i] = cancellationReason{Code: "None"}
		old := op.t.get(op.key)
		if op.kind == "Update" {
			_, item, err := op.t.update(op.key, op.cond, op.actions, op.rv)
			if err != nil {
				reasons[i] = reason(err)
				if reasons[i].Code != "ConditionalCheckFailed" {
					return nil, err
				}
				failed = true
				continue
			}
			results[i] = item
			continue
		}
		if op.cond != nil && !evalCond(old, op.cond) {
			failed = true
			reasons[i] = cancellationReason{Code: "ConditionalCheckFailed", Message: "The conditional request failed"}
			if op.rv == "ALL_OLD" {
				reasons[i].Item = old
			}
		}
	}
	if failed {
		codes := make([]string, len(reasons))
		for i, r := range reasons {
			codes[i] = r.Code
		}
		return nil, &awsproto.JSONError{
			Err: ddbError("TransactionCanceledException",
				"Transaction cancelled, please refer cancellation reasons for specific reasons [%s]", strings.Join(codes, ", ")),
			Fields: map[string]interface{}{"CancellationReasons": reasons},
		}
	}
	units := map[*table]float64{}
	for i, op := range ops {
		switch op.kind {
		case "Put":
			units[op.t] += 2 * writeUnits(op.t.get(op.key), op.item)
			op.t.put(op.item)
		case "Delete":
			units[op.t] += 2 * writeUnits(op.t.get(op.key))
			op.t.remove(op.key)
		case "Update":
			units[op.t] += 2 * writeUnits(op.t.get(op.key), results[i])
			op.t.put(results[i])
		case "ConditionCheck":
			units[op.t] += 2 * readUnits(itemSize(op.t.get(op.key)), true)
		}
	}
	if in.ClientRequestToken != "" {
		s.tokens[in.ClientRequestToken] = now.Add(10 * time.Minute)
	}
	out := struct {
		ConsumedCapacity []*consumed `json:",omitempty"`
	}{}
	for t, u := range units {
		if c := capacity(t, in.ReturnConsumedCapacity, u); c != nil {
			out.ConsumedCapacity = append(out.ConsumedCapacity, c)
		}
	}
	return out, nil
}

// reason converts a failed update into a cancellation reason.
func reason(err error) cancellationReason {
	r := cancellationReason{Code: "ConditionalCheckFailed", Message: "The conditional request failed"}
	switch e := err.(type) {
	case *awsproto.JSONError:
		if item, ok := e.Fields["Item"].(Item); ok {
			r.Item = item
		}
		return r
	case *awsproto.Error:
		if e.Code == "ConditionalCheckFailedException" {
			return r
		}
	}
	return cancellationReason{Code: "ValidationError", Message: awsMessage(err)}
}

// prepareWrite validates one transactional operation. Callers hold s.mu.
func (s *Service) prepareWrite(ti transactWriteItem) (pendingWrite, error) {
	var op pendingWrite
	var params *exprParams
	var table string
	n := 0
	if c := ti.ConditionCheck; c != nil {
		n++
		op.kind, table, op.key, params, op.rv = "ConditionCheck", c.TableName, c.Key, &c.exprParams, c.ReturnValuesOnConditionCheckFailure
		if c.ConditionExpression == "" {
			return op, validation("The ConditionCheck operation requires a ConditionExpression")
		}
	}
	if p := ti.Put; p != nil {
		n++
		op.kind, table, op.item, params, op.rv = "Put", p.TableName, p.Item, &p.exprParams, p.ReturnValuesOnConditionCheckFailure
	}
	if d := ti.Delete; d != nil {
		n++
		op.kind, table, op.key, params, op.rv = "Delete", d.TableName, d.Key, &d.exprParams, d.ReturnValuesOnConditionCheckFailure
	}
	var updateExpr string
	if u := ti.Update; u != nil {
		n++
		op.kind, table, op.key, params, op.rv = "Update", u.TableName, u.Key, &u.exprParams, u.ReturnValuesOnConditionCheckFailure
		updateExpr = u.UpdateExpression
	}
	if n != 1 {
		return op, validation("TransactItems can only contain one of Check, Put, Update or Delete")
	}
	t, err := s.table(table)
	if err != nil {
		return op, err
	}
	op.t = t
	ctx, c, err := params.prepare()
	if err != nil {
		return op, err
	}
	op.cond = c
	switch op.kind {
	case "Put":
		if err := t.checkItem(op.item); err != nil {
			return op, err
		}
		op.key = t.keyOf(op.item)
	case "Update":
		if updateExpr == "" {
			return op, validation("1 validation error detected: Value null at 'transactItems.member.update.updateExpression' failed to satisfy constraint: Member must not be null")
		}
		if op.actions, err = ctx.parseUpdate(updateExpr); err != nil {
			return op, err
		}
		fallthrough
	default:
		if err := t.checkKey(op.key); err != nil {
			return op, err
		}
	}
	return op, ctx.checkUnused()
}
