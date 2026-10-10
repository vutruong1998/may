package dynamodb

import "sort"

// Legacy (pre-expression) parameters are translated into the same AST the
// expression parser produces. Older SDK mappers still send them.

type legacyCondition struct {
	ComparisonOperator string
	AttributeValueList []*AV
}

type expectedAttr struct {
	Value              *AV
	Exists             *bool
	ComparisonOperator string
	AttributeValueList []*AV
}

type attributeUpdate struct {
	Value  *AV
	Action string
}

// legacyCompare builds the condition for one attribute.
func legacyCompare(name string, op string, args []*AV) (cond, error) {
	for _, v := range args {
		if err := normalize(v); err != nil {
			return nil, err
		}
	}
	p := pathOperand{path{{name: name}}}
	want := map[string]int{"EQ": 1, "NE": 1, "LE": 1, "LT": 1, "GE": 1, "GT": 1, "NOT_NULL": 0, "NULL": 0,
		"CONTAINS": 1, "NOT_CONTAINS": 1, "BEGINS_WITH": 1, "BETWEEN": 2}
	n, known := want[op]
	if op == "IN" {
		if len(args) == 0 {
			return nil, invalidParam("Invalid number of argument(s) for the IN ComparisonOperator")
		}
	} else if !known {
		return nil, validation("1 validation error detected: Value '%s' at 'comparisonOperator' failed to satisfy constraint: Member must satisfy enum value set: [IN, NULL, BETWEEN, LT, NOT_CONTAINS, EQ, GT, NOT_NULL, NE, LE, BEGINS_WITH, GE, CONTAINS]", op)
	} else if len(args) != n {
		return nil, invalidParam("Invalid number of argument(s) for the %s ComparisonOperator", op)
	}
	v := func(i int) operand { return valueOperand{args[i]} }
	switch op {
	case "EQ", "NE", "LE", "LT", "GE", "GT":
		sym := map[string]string{"EQ": "=", "NE": "<>", "LE": "<=", "LT": "<", "GE": ">=", "GT": ">"}[op]
		return compareCond{sym, p, v(0)}, nil
	case "NOT_NULL":
		return funcCond{"attribute_exists", []operand{p}}, nil
	case "NULL":
		return funcCond{"attribute_not_exists", []operand{p}}, nil
	case "CONTAINS":
		return funcCond{"contains", []operand{p, v(0)}}, nil
	case "NOT_CONTAINS":
		return andCond{funcCond{"attribute_exists", []operand{p}}, notCond{funcCond{"contains", []operand{p, v(0)}}}}, nil
	case "BEGINS_WITH":
		return funcCond{"begins_with", []operand{p, v(0)}}, nil
	case "BETWEEN":
		return betweenCond{p, v(0), v(1)}, nil
	}
	list := make([]operand, len(args))
	for i := range args {
		list[i] = v(i)
	}
	return inCond{p, list}, nil
}

// combine joins conditions with AND (default) or OR, in attribute order so
// evaluation is deterministic.
func combine(conds map[string]cond, op string) (cond, error) {
	if op != "" && op != "AND" && op != "OR" {
		return nil, validation("1 validation error detected: Value '%s' at 'conditionalOperator' failed to satisfy constraint: Member must satisfy enum value set: [AND, OR]", op)
	}
	names := make([]string, 0, len(conds))
	for n := range conds {
		names = append(names, n)
	}
	sort.Strings(names)
	var out cond
	for _, n := range names {
		switch {
		case out == nil:
			out = conds[n]
		case op == "OR":
			out = orCond{out, conds[n]}
		default:
			out = andCond{out, conds[n]}
		}
	}
	return out, nil
}

func expectedCond(expected map[string]expectedAttr, op string) (cond, error) {
	conds := map[string]cond{}
	for name, e := range expected {
		var c cond
		var err error
		switch {
		case e.ComparisonOperator != "":
			if e.Value != nil || e.Exists != nil {
				return nil, invalidParam("Value or Exists cannot be used with ComparisonOperator for Attribute: %s", name)
			}
			c, err = legacyCompare(name, e.ComparisonOperator, e.AttributeValueList)
		case e.Exists != nil && !*e.Exists:
			if e.Value != nil {
				return nil, invalidParam("Cannot expect an attribute to have a specified value while expecting it to not exist")
			}
			c, err = legacyCompare(name, "NULL", nil)
		case e.Value != nil:
			c, err = legacyCompare(name, "EQ", []*AV{e.Value})
		default:
			return nil, invalidParam("Exists is set to TRUE for attribute (%s), Value must also be set", name)
		}
		if err != nil {
			return nil, err
		}
		conds[name] = c
	}
	return combine(conds, op)
}

func filterCond(filter map[string]legacyCondition, op string) (cond, error) {
	conds := map[string]cond{}
	for name, f := range filter {
		c, err := legacyCompare(name, f.ComparisonOperator, f.AttributeValueList)
		if err != nil {
			return nil, err
		}
		conds[name] = c
	}
	return combine(conds, op)
}

func legacyUpdates(updates map[string]attributeUpdate) ([]updateAction, error) {
	var out []updateAction
	for _, name := range sortedKeysOf(updates) {
		u := updates[name]
		p := path{{name: name}}
		if u.Value != nil {
			if err := normalize(u.Value); err != nil {
				return nil, err
			}
		}
		switch u.Action {
		case "", "PUT":
			if u.Value == nil {
				return nil, invalidParam("Only DELETE action is allowed when no attribute value is specified")
			}
			out = append(out, updateAction{"SET", p, valueOperand{u.Value}})
		case "DELETE":
			if u.Value == nil {
				out = append(out, updateAction{kind: "REMOVE", p: p})
			} else {
				out = append(out, updateAction{"DELETE", p, valueOperand{u.Value}})
			}
		case "ADD":
			if u.Value == nil {
				return nil, invalidParam("Only DELETE action is allowed when no attribute value is specified")
			}
			if u.Value.L != nil {
				// Legacy ADD appends to lists.
				out = append(out, updateAction{"SET", p, ifNotExistsAppend(p, u.Value)})
				continue
			}
			out = append(out, updateAction{"ADD", p, valueOperand{u.Value}})
		default:
			return nil, validation("1 validation error detected: Value '%s' at 'attributeUpdates.%s.member.action' failed to satisfy constraint: Member must satisfy enum value set: [ADD, PUT, DELETE]", u.Action, name)
		}
	}
	return out, nil
}

func ifNotExistsAppend(p path, v *AV) operand {
	return listAppendOperand{ifNotExistsOperand{p, valueOperand{&AV{L: []*AV{}}}}, valueOperand{v}}
}
