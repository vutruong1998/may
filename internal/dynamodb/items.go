package dynamodb

import (
	"math"
	"net/http"

	"github.com/maycloud/may/internal/awsproto"
)

// exprParams are the expression parameters shared by item operations.
type exprParams struct {
	ConditionExpression       string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]*AV
	// Legacy equivalents.
	Expected            map[string]expectedAttr
	ConditionalOperator string
}

type returnParams struct {
	ReturnValues                        string
	ReturnConsumedCapacity              string
	ReturnItemCollectionMetrics         string
	ReturnValuesOnConditionCheckFailure string
}

type putInput struct {
	TableName string
	Item      Item
	exprParams
	returnParams
}

type getInput struct {
	TableName                string
	Key                      Item
	ConsistentRead           bool
	ProjectionExpression     string
	ExpressionAttributeNames map[string]string
	AttributesToGet          []string
	ReturnConsumedCapacity   string
}

type deleteInput struct {
	TableName string
	Key       Item
	exprParams
	returnParams
}

type updateInput struct {
	TableName        string
	Key              Item
	UpdateExpression string
	AttributeUpdates map[string]attributeUpdate
	exprParams
	returnParams
}

type itemOutput struct {
	Attributes       Item      `json:",omitempty"`
	ConsumedCapacity *consumed `json:",omitempty"`
}

type consumed struct {
	TableName     string
	CapacityUnits float64
	Table         *struct{ CapacityUnits float64 } `json:",omitempty"`
}

func (s *Service) registerItems() {
	handle(s, "PutItem", s.putItem)
	handle(s, "GetItem", s.getItem)
	handle(s, "DeleteItem", s.deleteItem)
	handle(s, "UpdateItem", s.updateItem)
}

// capacity reports consumed capacity when the caller asked for it.
func capacity(t *table, mode string, units float64) *consumed {
	switch mode {
	case "TOTAL":
		return &consumed{TableName: t.name, CapacityUnits: units}
	case "INDEXES":
		return &consumed{TableName: t.name, CapacityUnits: units, Table: &struct{ CapacityUnits float64 }{units}}
	}
	return nil
}

func writeUnits(items ...Item) float64 {
	size := 0
	for _, it := range items {
		if n := itemSize(it); n > size {
			size = n
		}
	}
	return math.Max(1, math.Ceil(float64(size)/1024))
}

func readUnits(size int, consistent bool) float64 {
	u := math.Max(1, math.Ceil(float64(size)/4096))
	if !consistent {
		u /= 2
	}
	return u
}

// condition builds the write condition from either ConditionExpression or
// the legacy Expected map; nil means unconditional.
func (p *exprParams) condition(ctx *exprContext) (cond, error) {
	if p.ConditionExpression != "" && len(p.Expected) > 0 {
		return nil, mixedParams("Expected", "ConditionExpression")
	}
	if len(p.Expected) > 0 {
		return expectedCond(p.Expected, p.ConditionalOperator)
	}
	if p.ConditionExpression == "" {
		return nil, nil
	}
	c, err := ctx.parseCondition("ConditionExpression", p.ConditionExpression)
	if err != nil {
		return nil, err
	}
	return c, validateCond(c, "ConditionExpression")
}

func mixedParams(legacy, expr string) error {
	return validation("Can not use both expression and non-expression parameters in the same request: Non-expression parameters: {%s} Expression parameters: {%s}", legacy, expr)
}

func checkReturnValues(rv string, allowed ...string) error {
	if rv == "" {
		return nil
	}
	for _, a := range allowed {
		if rv == a {
			return nil
		}
	}
	return validation("Return values set to invalid value")
}

// errConditionFailed carries the old item when the caller asked for it.
func errConditionFailed(old Item, rv string) error {
	e := ddbError("ConditionalCheckFailedException", "The conditional request failed")
	if rv == "ALL_OLD" && old != nil {
		return &awsproto.JSONError{Err: e, Fields: map[string]interface{}{"Item": old}}
	}
	return e
}

// writeContext parses placeholders and the condition for a single write.
func (p *exprParams) prepare() (*exprContext, cond, error) {
	if len(p.Expected) > 0 && (len(p.ExpressionAttributeNames) > 0 || len(p.ExpressionAttributeValues) > 0) {
		return nil, nil, mixedParams("Expected", "ExpressionAttributeNames, ExpressionAttributeValues")
	}
	ctx, err := newExprContext(p.ExpressionAttributeNames, p.ExpressionAttributeValues)
	if err != nil {
		return nil, nil, err
	}
	c, err := p.condition(ctx)
	return ctx, c, err
}

func (s *Service) putItem(_ *http.Request, in *putInput) (interface{}, error) {
	if err := checkReturnValues(in.ReturnValues, "NONE", "ALL_OLD"); err != nil {
		return nil, err
	}
	ctx, c, err := in.prepare()
	if err != nil {
		return nil, err
	}
	if err := ctx.checkUnused(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	if in.Item == nil {
		return nil, validation("1 validation error detected: Value null at 'item' failed to satisfy constraint: Member must not be null")
	}
	if err := t.checkItem(in.Item); err != nil {
		return nil, err
	}
	old := t.get(t.keyOf(in.Item))
	if c != nil && !evalCond(old, c) {
		return nil, errConditionFailed(old, in.ReturnValuesOnConditionCheckFailure)
	}
	t.put(in.Item)
	out := itemOutput{ConsumedCapacity: capacity(t, in.ReturnConsumedCapacity, writeUnits(old, in.Item))}
	if in.ReturnValues == "ALL_OLD" {
		out.Attributes = old
	}
	return out, nil
}

// projection builds the attribute paths to return from either
// ProjectionExpression or the legacy AttributesToGet; nil means all.
func projection(ctx *exprContext, expr string, attrs []string) ([]path, error) {
	if expr != "" && len(attrs) > 0 {
		return nil, mixedParams("AttributesToGet", "ProjectionExpression")
	}
	if len(attrs) > 0 {
		out := make([]path, len(attrs))
		for i, a := range attrs {
			out[i] = path{{name: a}}
		}
		return out, nil
	}
	if expr == "" {
		return nil, nil
	}
	return ctx.parseProjection(expr)
}

func (s *Service) getItem(_ *http.Request, in *getInput) (interface{}, error) {
	ctx, err := newExprContext(in.ExpressionAttributeNames, nil)
	if err != nil {
		return nil, err
	}
	paths, err := projection(ctx, in.ProjectionExpression, in.AttributesToGet)
	if err != nil {
		return nil, err
	}
	if err := ctx.checkUnused(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	if err := t.checkKey(in.Key); err != nil {
		return nil, err
	}
	item := t.get(in.Key)
	out := struct {
		Item             Item      `json:",omitempty"`
		ConsumedCapacity *consumed `json:",omitempty"`
	}{ConsumedCapacity: capacity(t, in.ReturnConsumedCapacity, readUnits(itemSize(item), in.ConsistentRead))}
	if item != nil {
		out.Item = item
		if paths != nil {
			out.Item = project(item, paths)
		}
	}
	return out, nil
}

func (s *Service) deleteItem(_ *http.Request, in *deleteInput) (interface{}, error) {
	if err := checkReturnValues(in.ReturnValues, "NONE", "ALL_OLD"); err != nil {
		return nil, err
	}
	ctx, c, err := in.prepare()
	if err != nil {
		return nil, err
	}
	if err := ctx.checkUnused(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	if err := t.checkKey(in.Key); err != nil {
		return nil, err
	}
	old := t.get(in.Key)
	if c != nil && !evalCond(old, c) {
		return nil, errConditionFailed(old, in.ReturnValuesOnConditionCheckFailure)
	}
	t.remove(in.Key)
	out := itemOutput{ConsumedCapacity: capacity(t, in.ReturnConsumedCapacity, writeUnits(old))}
	if in.ReturnValues == "ALL_OLD" {
		out.Attributes = old
	}
	return out, nil
}

// updateActions builds the actions from UpdateExpression or the legacy
// AttributeUpdates.
func (in *updateInput) actions(ctx *exprContext) ([]updateAction, error) {
	if in.UpdateExpression != "" && len(in.AttributeUpdates) > 0 {
		return nil, mixedParams("AttributeUpdates", "UpdateExpression")
	}
	if len(in.AttributeUpdates) > 0 {
		return legacyUpdates(in.AttributeUpdates)
	}
	if in.UpdateExpression == "" {
		return nil, nil
	}
	return ctx.parseUpdate(in.UpdateExpression)
}

func (s *Service) updateItem(_ *http.Request, in *updateInput) (interface{}, error) {
	if err := checkReturnValues(in.ReturnValues, "NONE", "ALL_OLD", "UPDATED_OLD", "ALL_NEW", "UPDATED_NEW"); err != nil {
		return nil, err
	}
	ctx, c, err := in.prepare()
	if err != nil {
		return nil, err
	}
	actions, err := in.actions(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.checkUnused(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.table(in.TableName)
	if err != nil {
		return nil, err
	}
	old, item, err := t.update(in.Key, c, actions, in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, err
	}
	t.put(item)
	out := itemOutput{ConsumedCapacity: capacity(t, in.ReturnConsumedCapacity, writeUnits(old, item))}
	switch in.ReturnValues {
	case "ALL_OLD":
		out.Attributes = old
	case "ALL_NEW":
		out.Attributes = item
	case "UPDATED_OLD":
		out.Attributes = project(old, updatedPaths(actions))
	case "UPDATED_NEW":
		out.Attributes = project(item, updatedPaths(actions))
	}
	if len(out.Attributes) == 0 {
		out.Attributes = nil
	}
	return out, nil
}

// update computes the new item for an UpdateItem (or transactional Update)
// without storing it. Callers hold s.mu.
func (t *table) update(key Item, c cond, actions []updateAction, rvOnFailure string) (old, item Item, err error) {
	if err := t.checkKey(key); err != nil {
		return nil, nil, err
	}
	for _, a := range actions {
		for _, k := range t.key.names() {
			if a.p[0].name == k {
				return nil, nil, invalidParam("Cannot update attribute %s. This attribute is part of the key", k)
			}
		}
	}
	old = t.get(key)
	if c != nil && !evalCond(old, c) {
		return nil, nil, errConditionFailed(old, rvOnFailure)
	}
	base := old
	if base == nil {
		base = Item{}
		for k, v := range key {
			base[k] = v
		}
	}
	item, err = applyUpdate(base, actions)
	if err != nil {
		return nil, nil, err
	}
	if err := t.checkItem(item); err != nil {
		return nil, nil, err
	}
	return old, item, nil
}
