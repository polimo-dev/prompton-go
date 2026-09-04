package liquid

import (
	"encoding/json"
	"errors"
	"strings"
)

// Engine selects how a prompt version's source is treated.
type Engine string

// The two engines a prompt version can declare.
const (
	EngineLiquid Engine = "liquid"
	EngineRaw    Engine = "raw"
)

// Render parses and renders src with vars.
//
// A variable is missing when its key is absent from vars. A key present with a
// null value is not missing: it renders as the empty string and the default
// filter replaces it. Missing variables are reported at output positions, in a
// for enumerable, in an unless condition and as an assign source; a branch that
// does not execute is never looked at, and — matching the reference
// implementation — a variable read only by an if/elsif condition is not
// reported either.
func Render(src string, vars map[string]interface{}, engine Engine) (string, *Error) {
	if engine == EngineRaw {
		return src, nil
	}
	tmpl, err := Parse(src)
	if err != nil {
		return "", err
	}
	return tmpl.Render(vars)
}

// Render renders a parsed template.
func (t *Template) Render(vars map[string]interface{}) (string, *Error) {
	sc := newScope(NormalizeMap(vars))
	var sb strings.Builder
	if err := renderNodes(t.nodes, sc, &sb); err != nil {
		if lerr, ok := err.(*Error); ok {
			return "", lerr
		}
		return "", renderErr("%v", err)
	}
	return sb.String(), nil
}

// ---------------------------------------------------------------------------
// scope

type scope struct {
	globals map[string]interface{}
	locals  []map[string]interface{}
	// pending holds the missing-variable error accumulated while evaluating a
	// condition. It only surfaces if that branch is taken, which is what makes
	// `{% if absent == "x" %}` render empty while `{% unless absent %}` fails.
	pending *Error
}

type evalMode int

const (
	modeStrict evalMode = iota
	modeCollect
)

func newScope(vars map[string]interface{}) *scope {
	return &scope{globals: vars}
}

func (s *scope) push(m map[string]interface{}) { s.locals = append(s.locals, m) }

func (s *scope) pop() { s.locals = s.locals[:len(s.locals)-1] }

func (s *scope) lookup(name string) (interface{}, bool) {
	for i := len(s.locals) - 1; i >= 0; i-- {
		if v, ok := s.locals[i][name]; ok {
			return v, true
		}
	}
	v, ok := s.globals[name]
	return v, ok
}

func (s *scope) assign(name string, v interface{}) { s.globals[name] = v }

// ---------------------------------------------------------------------------
// control-flow sentinels

var errBreak = errors.New("liquid: break")
var errContinue = errors.New("liquid: continue")

func renderNodes(nodes []node, sc *scope, sb *strings.Builder) error {
	for _, n := range nodes {
		if err := renderNode(n, sc, sb); err != nil {
			return err
		}
	}
	return nil
}

func renderNode(n node, sc *scope, sb *strings.Builder) error {
	switch x := n.(type) {
	case *textNode:
		sb.WriteString(x.text)
		return nil

	case *outputNode:
		v, err := evalOutput(x.expr, sc, modeStrict)
		if err != nil {
			return err
		}
		sb.WriteString(ToString(v))
		return nil

	case *assignNode:
		v, err := evalOutput(x.expr, sc, modeStrict)
		if err != nil {
			return err
		}
		sc.assign(x.target, v)
		return nil

	case *ifNode:
		for _, br := range x.branches {
			taken, err := evalCondition(br.cond, sc)
			if err != nil {
				return err
			}
			if taken {
				if p := sc.takePending(); p != nil {
					return p
				}
				return renderNodes(br.body, sc, sb)
			}
		}
		sc.takePending()
		return renderNodes(x.elseBody, sc, sb)

	case *unlessNode:
		taken, err := evalCondition(x.cond, sc)
		if err != nil {
			return err
		}
		if !taken {
			if p := sc.takePending(); p != nil {
				return p
			}
			return renderNodes(x.body, sc, sb)
		}
		sc.takePending()
		return renderNodes(x.elseBody, sc, sb)

	case *forNode:
		return renderFor(x, sc, sb)

	case *breakNode:
		return errBreak

	case *continueNode:
		return errContinue
	}
	return renderErr("unsupported node %T", n)
}

// evalCondition evaluates a branch condition, collecting rather than raising a
// missing-variable error so the caller can drop it when the branch is untaken.
func evalCondition(e *expr, sc *scope) (bool, error) {
	sc.pending = nil
	v, err := eval(e, sc, modeCollect)
	if err != nil {
		return false, err
	}
	return Truthy(v), nil
}

func (s *scope) takePending() *Error {
	p := s.pending
	s.pending = nil
	return p
}

func renderFor(x *forNode, sc *scope, sb *strings.Builder) error {
	seq, err := eval(x.seq, sc, modeStrict)
	if err != nil {
		return err
	}
	items := iterable(seq)
	if len(items) == 0 {
		return renderNodes(x.elseBody, sc, sb)
	}
	n := len(items)
	for i, item := range items {
		frame := map[string]interface{}{
			x.varName: item,
			"forloop": map[string]interface{}{
				"index":   json.Number(itoa(i + 1)),
				"index0":  json.Number(itoa(i)),
				"rindex":  json.Number(itoa(n - i)),
				"rindex0": json.Number(itoa(n - i - 1)),
				"first":   i == 0,
				"last":    i == n-1,
				"length":  json.Number(itoa(n)),
			},
		}
		sc.push(frame)
		err := renderNodes(x.body, sc, sb)
		sc.pop()
		if err != nil {
			if errors.Is(err, errBreak) {
				return nil
			}
			if errors.Is(err, errContinue) {
				continue
			}
			return err
		}
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func iterable(v interface{}) []interface{} {
	switch x := v.(type) {
	case nil:
		return nil
	case []interface{}:
		return x
	case map[string]interface{}:
		out := make([]interface{}, 0, len(x))
		for _, k := range sortedKeys(x) {
			out = append(out, []interface{}{k, x[k]})
		}
		return out
	default:
		return []interface{}{v}
	}
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// ---------------------------------------------------------------------------
// evaluation

func evalOutput(oe *outputExpr, sc *scope, mode evalMode) (interface{}, error) {
	v, err := eval(oe.base, sc, mode)
	if err != nil {
		return nil, err
	}
	for _, f := range oe.filters {
		v, err = applyFilter(f, v, sc, mode)
		if err != nil {
			return nil, err
		}
	}
	return v, nil
}

func eval(e *expr, sc *scope, mode evalMode) (interface{}, error) {
	switch e.kind {
	case exprLiteral:
		return e.literal, nil

	case exprVar:
		v, found, err := lookupPath(e, sc, mode)
		if err != nil {
			return nil, err
		}
		if !found {
			if mode == modeStrict {
				return nil, missingErr(e.name)
			}
			if sc.pending == nil {
				sc.pending = missingErr(e.name)
			}
			return nil, nil
		}
		return v, nil

	case exprAnd:
		l, err := eval(e.lhs, sc, mode)
		if err != nil {
			return nil, err
		}
		if !Truthy(l) {
			return false, nil
		}
		r, err := eval(e.rhs, sc, mode)
		if err != nil {
			return nil, err
		}
		return Truthy(r), nil

	case exprOr:
		l, err := eval(e.lhs, sc, mode)
		if err != nil {
			return nil, err
		}
		if Truthy(l) {
			return true, nil
		}
		r, err := eval(e.rhs, sc, mode)
		if err != nil {
			return nil, err
		}
		return Truthy(r), nil

	case exprBinary:
		l, err := eval(e.lhs, sc, mode)
		if err != nil {
			return nil, err
		}
		r, err := eval(e.rhs, sc, mode)
		if err != nil {
			return nil, err
		}
		return compare(e.op, l, r)
	}
	return nil, renderErr("unsupported expression")
}

func lookupPath(e *expr, sc *scope, mode evalMode) (interface{}, bool, error) {
	if len(e.path) == 0 {
		return nil, false, nil
	}
	cur, ok := sc.lookup(e.path[0].name)
	if !ok {
		return nil, false, nil
	}
	for _, seg := range e.path[1:] {
		switch {
		case seg.dynamic != nil:
			key, err := eval(seg.dynamic, sc, mode)
			if err != nil {
				return nil, false, err
			}
			next, found := indexValue(cur, key)
			if !found {
				return nil, false, nil
			}
			cur = next
		case seg.isIndex:
			next, found := indexValue(cur, json.Number(itoa(seg.index)))
			if !found {
				return nil, false, nil
			}
			cur = next
		default:
			next, found := indexValue(cur, seg.name)
			if !found {
				return nil, false, nil
			}
			cur = next
		}
	}
	return cur, true, nil
}

func indexValue(container, key interface{}) (interface{}, bool) {
	switch c := container.(type) {
	case map[string]interface{}:
		v, ok := c[ToString(key)]
		return v, ok
	case []interface{}:
		switch k := key.(type) {
		case json.Number:
			n, err := k.Int64()
			if err != nil {
				return nil, false
			}
			i := int(n)
			if i < 0 {
				i += len(c)
			}
			if i < 0 || i >= len(c) {
				return nil, false
			}
			return c[i], true
		case string:
			switch k {
			case "size":
				return json.Number(itoa(len(c))), true
			case "first":
				if len(c) == 0 {
					return nil, false
				}
				return c[0], true
			case "last":
				if len(c) == 0 {
					return nil, false
				}
				return c[len(c)-1], true
			}
		}
		return nil, false
	case string:
		if ToString(key) == "size" {
			return json.Number(itoa(sizeOf(c))), true
		}
		return nil, false
	default:
		return nil, false
	}
}

func compare(op string, l, r interface{}) (interface{}, error) {
	switch op {
	case "==":
		return valuesEqual(l, r), nil
	case "!=":
		return !valuesEqual(l, r), nil
	case "contains":
		return contains(l, r), nil
	case ">", "<", ">=", "<=":
		ln, lok := numberOf(l)
		rn, rok := numberOf(r)
		if lok && rok {
			switch op {
			case ">":
				return ln > rn, nil
			case "<":
				return ln < rn, nil
			case ">=":
				return ln >= rn, nil
			case "<=":
				return ln <= rn, nil
			}
		}
		ls, lok2 := l.(string)
		rs, rok2 := r.(string)
		if lok2 && rok2 {
			switch op {
			case ">":
				return ls > rs, nil
			case "<":
				return ls < rs, nil
			case ">=":
				return ls >= rs, nil
			case "<=":
				return ls <= rs, nil
			}
		}
		return false, nil
	}
	return nil, renderErr("unsupported operator %q", op)
}

func valuesEqual(l, r interface{}) bool {
	if _, ok := r.(emptyMarker); ok {
		return blank(l)
	}
	if _, ok := l.(emptyMarker); ok {
		return blank(r)
	}
	if l == nil || r == nil {
		return l == nil && r == nil
	}
	ln, lok := numberOf(l)
	rn, rok := numberOf(r)
	if lok && rok {
		return ln == rn
	}
	switch lv := l.(type) {
	case string:
		rv, ok := r.(string)
		return ok && lv == rv
	case bool:
		rv, ok := r.(bool)
		return ok && lv == rv
	case []interface{}:
		rv, ok := r.([]interface{})
		if !ok || len(lv) != len(rv) {
			return false
		}
		for i := range lv {
			if !valuesEqual(lv[i], rv[i]) {
				return false
			}
		}
		return true
	}
	return false
}

func contains(haystack, needle interface{}) bool {
	switch h := haystack.(type) {
	case string:
		return strings.Contains(h, ToString(needle))
	case []interface{}:
		for _, el := range h {
			if valuesEqual(el, needle) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// filters

func applyFilter(f filterCall, v interface{}, sc *scope, mode evalMode) (interface{}, error) {
	args := make([]interface{}, 0, len(f.args))
	for _, a := range f.args {
		av, err := eval(a, sc, mode)
		if err != nil {
			return nil, err
		}
		args = append(args, av)
	}
	switch f.name {
	case "size":
		return json.Number(itoa(sizeOf(v))), nil
	case "join":
		sep := defaultSeparator
		if len(args) > 0 {
			sep = ToString(args[0])
		}
		list, ok := v.([]interface{})
		if !ok {
			return ToString(v), nil
		}
		parts := make([]string, len(list))
		for i, el := range list {
			parts[i] = ToString(el)
		}
		return strings.Join(parts, sep), nil
	case "default":
		if blank(v) {
			if len(args) > 0 {
				return args[0], nil
			}
			return "", nil
		}
		return v, nil
	default:
		// The filter whitelist is enforced by Lint and by the server at commit
		// time, so a template using anything else can never reach a snapshot.
		return nil, renderErr("filter %q is not allowed (allowed: size, join, default)", f.name)
	}
}
