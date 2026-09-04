package liquid

import (
	"sort"
	"strings"
)

// LintReason is one whitelist violation. Kind is "whitespace_control",
// "disallowed_tag", "disallowed_filter" or "parse".
type LintReason struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// builtinVariables are injected by the renderer, not supplied by the caller.
var builtinVariables = map[string]bool{"forloop": true}

var allowedTagNames = map[string]bool{
	"for": true, "endfor": true,
	"if": true, "endif": true, "elsif": true, "else": true,
	"unless": true, "endunless": true,
	"assign": true, "break": true, "continue": true,
}

// Lint checks a template against the whitelist PromptOn enforces when a prompt
// version is committed: the six tags, the three filters, and no whitespace
// control. It returns nil when the template is acceptable.
//
// The server runs the same check, so a template that fails here can never reach
// a snapshot — which is why the renderer does not repeat the filter check.
func Lint(src string) []LintReason {
	var reasons []LintReason
	seen := map[LintReason]bool{}
	add := func(kind, value string) {
		r := LintReason{Kind: kind, Value: value}
		if seen[r] {
			return
		}
		seen[r] = true
		reasons = append(reasons, r)
	}

	for _, marker := range whitespaceControlMarkers(src) {
		add("whitespace_control", marker)
	}

	disallowed := false
	for _, name := range tagNames(src) {
		if !allowedTagNames[name] {
			add("disallowed_tag", name)
			disallowed = true
		}
	}
	if disallowed {
		return reasons
	}

	tmpl, err := Parse(src)
	if err != nil {
		add("parse", err.Message)
		return reasons
	}
	for _, f := range tmpl.filterNames() {
		if !allowedFilterSet[f] {
			add("disallowed_filter", f)
		}
	}
	return reasons
}

// whitespaceControlMarkers lists the whitespace-control markers used, in order
// of first appearance.
func whitespaceControlMarkers(src string) []string {
	var out []string
	i := 0
	for i < len(src) {
		if src[i] != '{' || i+1 >= len(src) || (src[i+1] != '{' && src[i+1] != '%') {
			i++
			continue
		}
		isTag := src[i+1] == '%'
		open, closer := "{{-", "}}"
		if isTag {
			open, closer = "{%-", "%}"
		}
		if i+2 < len(src) && src[i+2] == '-' {
			out = append(out, open)
		}
		end := strings.Index(src[i+2:], closer)
		if end < 0 {
			break
		}
		end += i + 2
		if end > i+2 && src[end-1] == '-' {
			if isTag {
				out = append(out, "-%}")
			} else {
				out = append(out, "-}}")
			}
		}
		i = end + 2
	}
	return out
}

// tagNames lists the tag names used, in order of appearance, including closing
// tags, so `{% capture %}…{% endcapture %}` reports both.
func tagNames(src string) []string {
	var out []string
	i := 0
	for i+1 < len(src) {
		if src[i] != '{' || src[i+1] != '%' {
			i++
			continue
		}
		j := i + 2
		for j < len(src) && (src[j] == '-' || src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
			j++
		}
		k := j
		for k < len(src) {
			c := src[k]
			if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
				k++
				continue
			}
			break
		}
		if k > j {
			out = append(out, src[j:k])
		}
		end := strings.Index(src[i+2:], "%}")
		if end < 0 {
			break
		}
		i = end + i + 4
	}
	return out
}

// Variables lists the top-level input variables a template reads, sorted and
// deduplicated. for-loop variables, assign targets and forloop are excluded.
func Variables(src string) []string {
	tmpl, err := Parse(src)
	if err != nil {
		return regexVariables(src)
	}
	referenced, bound := tmpl.variableNames()
	set := map[string]bool{}
	for _, name := range referenced {
		if bound[name] || builtinVariables[name] {
			continue
		}
		set[name] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func regexVariables(src string) []string {
	set := map[string]bool{}
	reserved := map[string]bool{"true": true, "false": true, "nil": true, "null": true, "empty": true, "blank": true, "and": true, "or": true, "contains": true, "in": true}
	toks, err := scanExpr(stripDelimiters(src))
	if err != nil {
		return nil
	}
	for i, t := range toks {
		if t.kind != "ident" || reserved[t.text] || builtinVariables[t.text] {
			continue
		}
		if i > 0 && toks[i-1].kind == "punct" && toks[i-1].text == "." {
			continue
		}
		set[t.text] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func stripDelimiters(src string) string {
	var sb strings.Builder
	i := 0
	for i < len(src) {
		if src[i] == '{' && i+1 < len(src) && (src[i+1] == '{' || src[i+1] == '%') {
			closer := "}}"
			if src[i+1] == '%' {
				closer = "%}"
			}
			end := strings.Index(src[i+2:], closer)
			if end < 0 {
				break
			}
			sb.WriteString(" ")
			sb.WriteString(strings.Trim(src[i+2:i+2+end], "-"))
			sb.WriteString(" ")
			i = i + 2 + end + 2
			continue
		}
		i++
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// AST walking

func (t *Template) filterNames() []string {
	var out []string
	seen := map[string]bool{}
	walkNodes(t.nodes, func(oe *outputExpr) {
		for _, f := range oe.filters {
			if !seen[f.name] {
				seen[f.name] = true
				out = append(out, f.name)
			}
		}
	}, nil, nil)
	return out
}

func (t *Template) variableNames() ([]string, map[string]bool) {
	var referenced []string
	bound := map[string]bool{}
	collect := func(e *expr) {
		if e == nil {
			return
		}
		walkExpr(e, func(v *expr) {
			if len(v.path) > 0 {
				referenced = append(referenced, v.path[0].name)
			}
		})
	}
	walkNodes(t.nodes, func(oe *outputExpr) {
		collect(oe.base)
		for _, f := range oe.filters {
			for _, a := range f.args {
				collect(a)
			}
		}
	}, collect, func(name string) { bound[name] = true })
	return referenced, bound
}

func walkNodes(nodes []node, onOutput func(*outputExpr), onExpr func(*expr), onBind func(string)) {
	for _, n := range nodes {
		switch x := n.(type) {
		case *outputNode:
			if onOutput != nil {
				onOutput(x.expr)
			}
		case *assignNode:
			if onBind != nil {
				onBind(x.target)
			}
			if onOutput != nil {
				onOutput(x.expr)
			}
		case *ifNode:
			for _, br := range x.branches {
				if onExpr != nil {
					onExpr(br.cond)
				}
				walkNodes(br.body, onOutput, onExpr, onBind)
			}
			walkNodes(x.elseBody, onOutput, onExpr, onBind)
		case *unlessNode:
			if onExpr != nil {
				onExpr(x.cond)
			}
			walkNodes(x.body, onOutput, onExpr, onBind)
			walkNodes(x.elseBody, onOutput, onExpr, onBind)
		case *forNode:
			if onBind != nil {
				onBind(x.varName)
			}
			if onExpr != nil {
				onExpr(x.seq)
			}
			walkNodes(x.body, onOutput, onExpr, onBind)
			walkNodes(x.elseBody, onOutput, onExpr, onBind)
		}
	}
}

func walkExpr(e *expr, fn func(*expr)) {
	if e == nil {
		return
	}
	switch e.kind {
	case exprVar:
		fn(e)
		for _, seg := range e.path {
			if seg.dynamic != nil {
				walkExpr(seg.dynamic, fn)
			}
		}
	case exprBinary, exprAnd, exprOr:
		walkExpr(e.lhs, fn)
		walkExpr(e.rhs, fn)
	}
}
