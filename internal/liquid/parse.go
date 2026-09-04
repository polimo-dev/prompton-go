package liquid

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Error categories. They are the vocabulary the cross-language conformance
// suite uses, so they are part of the contract rather than an implementation
// detail.
const (
	CategoryParse    = "parse_error"
	CategoryRender   = "render_error"
	CategoryMissing  = "missing_variable"
	defaultSeparator = " "
)

// Error is a template failure. Variable is set for CategoryMissing and carries
// the dotted path exactly as the template wrote it.
type Error struct {
	Category string
	Variable string
	Message  string
}

func (e *Error) Error() string {
	if e.Variable != "" {
		return fmt.Sprintf("%s: %s", e.Category, e.Variable)
	}
	return fmt.Sprintf("%s: %s", e.Category, e.Message)
}

func parseErr(format string, args ...interface{}) *Error {
	return &Error{Category: CategoryParse, Message: fmt.Sprintf(format, args...)}
}

func renderErr(format string, args ...interface{}) *Error {
	return &Error{Category: CategoryRender, Message: fmt.Sprintf(format, args...)}
}

func missingErr(name string) *Error {
	return &Error{Category: CategoryMissing, Variable: name, Message: "missing variable: " + name}
}

// AllowedTags is the tag whitelist, sorted.
func AllowedTags() []string {
	return []string{"assign", "break", "continue", "for", "if", "unless"}
}

// AllowedFilters is the filter whitelist.
func AllowedFilters() []string { return []string{"size", "join", "default"} }

var allowedFilterSet = map[string]bool{"size": true, "join": true, "default": true}

var blockTagSet = map[string]bool{
	"if": true, "endif": true, "elsif": true, "else": true,
	"unless": true, "endunless": true,
	"for": true, "endfor": true,
	"assign": true, "break": true, "continue": true,
}

// ---------------------------------------------------------------------------
// lexing

type tokenKind int

const (
	tokText tokenKind = iota
	tokOutput
	tokTag
)

type token struct {
	kind tokenKind
	body string
}

func lex(src string) ([]token, *Error) {
	var toks []token
	trimNextText := false
	i := 0
	for i < len(src) {
		open := -1
		isTag := false
		for j := i; j+1 < len(src); j++ {
			if src[j] == '{' && (src[j+1] == '{' || src[j+1] == '%') {
				open = j
				isTag = src[j+1] == '%'
				break
			}
		}
		if open < 0 {
			toks = appendText(toks, src[i:], trimNextText, false)
			return toks, nil
		}
		trimLeft := open+2 < len(src) && src[open+2] == '-'
		toks = appendText(toks, src[i:open], trimNextText, trimLeft)

		closer := "}}"
		if isTag {
			closer = "%}"
		}
		end := strings.Index(src[open+2:], closer)
		if end < 0 {
			return nil, parseErr("unexpected end of template: missing %q", closer)
		}
		end += open + 2
		body := src[open+2 : end]
		trimNextText = strings.HasSuffix(body, "-")
		body = strings.TrimSuffix(body, "-")
		if trimLeft {
			body = strings.TrimPrefix(body, "-")
		}
		body = strings.TrimSpace(body)
		if isTag {
			toks = append(toks, token{kind: tokTag, body: body})
		} else {
			toks = append(toks, token{kind: tokOutput, body: body})
		}
		i = end + len(closer)
	}
	return toks, nil
}

func appendText(toks []token, text string, trimLeading, trimTrailing bool) []token {
	if trimLeading {
		text = strings.TrimLeft(text, " \t\r\n")
	}
	if trimTrailing {
		text = strings.TrimRight(text, " \t\r\n")
	}
	if text == "" {
		return toks
	}
	return append(toks, token{kind: tokText, body: text})
}

// ---------------------------------------------------------------------------
// AST

type node interface{}

type textNode struct{ text string }

type outputNode struct{ expr *outputExpr }

type branch struct {
	cond *expr
	body []node
}

type ifNode struct {
	branches []branch
	elseBody []node
}

type unlessNode struct {
	cond     *expr
	body     []node
	elseBody []node
}

type forNode struct {
	varName  string
	seq      *expr
	body     []node
	elseBody []node
}

type assignNode struct {
	target string
	expr   *outputExpr
}

type breakNode struct{}

type continueNode struct{}

// Template is a parsed template ready to render.
type Template struct {
	nodes []node
	raw   string
}

// Source returns the template source.
func (t *Template) Source() string { return t.raw }

const (
	exprLiteral = iota
	exprVar
	exprBinary
	exprAnd
	exprOr
)

type pathSeg struct {
	name    string
	index   int
	dynamic *expr
	isIndex bool
}

type expr struct {
	kind    int
	literal interface{}
	path    []pathSeg
	name    string // dotted display name, used in missing-variable errors
	op      string
	lhs     *expr
	rhs     *expr
}

type filterCall struct {
	name string
	args []*expr
}

type outputExpr struct {
	base    *expr
	filters []filterCall
}

// ---------------------------------------------------------------------------
// parsing

type parser struct {
	toks []token
	pos  int
}

// Parse compiles a template. Any construct outside the allowed subset is a
// CategoryParse error.
func Parse(src string) (*Template, *Error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	nodes, stop, err := p.parseBlock(nil)
	if err != nil {
		return nil, err
	}
	if stop != "" {
		return nil, parseErr("Unexpected tag '%s'", stop)
	}
	return &Template{nodes: nodes, raw: src}, nil
}

// parseBlock reads nodes until one of the stop tags (or the end of input). It
// returns the tag name that stopped it, empty at end of input.
func (p *parser) parseBlock(stops map[string]bool) ([]node, string, *Error) {
	var nodes []node
	for p.pos < len(p.toks) {
		tok := p.toks[p.pos]
		switch tok.kind {
		case tokText:
			nodes = append(nodes, &textNode{text: tok.body})
			p.pos++
		case tokOutput:
			oe, err := parseOutputExpr(tok.body)
			if err != nil {
				return nil, "", err
			}
			nodes = append(nodes, &outputNode{expr: oe})
			p.pos++
		case tokTag:
			name, rest := splitTag(tok.body)
			if stops != nil && stops[name] {
				return nodes, name, nil
			}
			if !blockTagSet[name] {
				return nil, "", parseErr("Unexpected tag '%s'", name)
			}
			p.pos++
			n, err := p.parseTag(name, rest)
			if err != nil {
				return nil, "", err
			}
			if n != nil {
				nodes = append(nodes, n)
			}
		}
	}
	return nodes, "", nil
}

func splitTag(body string) (string, string) {
	body = strings.TrimSpace(body)
	i := strings.IndexAny(body, " \t\r\n")
	if i < 0 {
		return body, ""
	}
	return body[:i], strings.TrimSpace(body[i:])
}

var (
	ifStops     = map[string]bool{"elsif": true, "else": true, "endif": true}
	unlessStops = map[string]bool{"else": true, "endunless": true}
	forStops    = map[string]bool{"else": true, "endfor": true}
)

func (p *parser) parseTag(name, rest string) (node, *Error) {
	switch name {
	case "if":
		return p.parseIf(rest)
	case "unless":
		return p.parseUnless(rest)
	case "for":
		return p.parseFor(rest)
	case "assign":
		return parseAssign(rest)
	case "break":
		return &breakNode{}, nil
	case "continue":
		return &continueNode{}, nil
	default:
		// elsif/else/end* reaching here means they had no opening tag.
		return nil, parseErr("Unexpected tag '%s'", name)
	}
}

func (p *parser) parseIf(rest string) (node, *Error) {
	cond, err := parseCondition(rest)
	if err != nil {
		return nil, err
	}
	n := &ifNode{}
	for {
		body, stop, err := p.parseBlock(ifStops)
		if err != nil {
			return nil, err
		}
		n.branches = append(n.branches, branch{cond: cond, body: removeBlankText(body)})
		switch stop {
		case "":
			return nil, parseErr("Expected 'endif'")
		case "endif":
			p.pos++
			return n, nil
		case "elsif":
			_, r := splitTag(p.toks[p.pos].body)
			p.pos++
			cond, err = parseCondition(r)
			if err != nil {
				return nil, err
			}
		case "else":
			p.pos++
			elseBody, stop2, err := p.parseBlock(map[string]bool{"endif": true})
			if err != nil {
				return nil, err
			}
			if stop2 != "endif" {
				return nil, parseErr("Expected 'endif'")
			}
			p.pos++
			n.elseBody = removeBlankText(elseBody)
			return n, nil
		}
	}
}

func (p *parser) parseUnless(rest string) (node, *Error) {
	cond, err := parseCondition(rest)
	if err != nil {
		return nil, err
	}
	body, stop, err := p.parseBlock(unlessStops)
	if err != nil {
		return nil, err
	}
	n := &unlessNode{cond: cond, body: removeBlankText(body)}
	switch stop {
	case "endunless":
		p.pos++
		return n, nil
	case "else":
		p.pos++
		elseBody, stop2, err := p.parseBlock(map[string]bool{"endunless": true})
		if err != nil {
			return nil, err
		}
		if stop2 != "endunless" {
			return nil, parseErr("Expected 'endunless'")
		}
		p.pos++
		n.elseBody = removeBlankText(elseBody)
		return n, nil
	default:
		return nil, parseErr("Expected 'endunless'")
	}
}

func (p *parser) parseFor(rest string) (node, *Error) {
	fields := strings.Fields(rest)
	if len(fields) < 3 || fields[1] != "in" {
		return nil, parseErr("malformed for tag: %q", rest)
	}
	seq, err := parseOperandString(strings.Join(fields[2:], " "))
	if err != nil {
		return nil, err
	}
	body, stop, err := p.parseBlock(forStops)
	if err != nil {
		return nil, err
	}
	n := &forNode{varName: fields[0], seq: seq, body: removeBlankText(body)}
	switch stop {
	case "endfor":
		p.pos++
		return n, nil
	case "else":
		p.pos++
		elseBody, stop2, err := p.parseBlock(map[string]bool{"endfor": true})
		if err != nil {
			return nil, err
		}
		if stop2 != "endfor" {
			return nil, parseErr("Expected 'endfor'")
		}
		p.pos++
		n.elseBody = removeBlankText(elseBody)
		return n, nil
	default:
		return nil, parseErr("Expected 'endfor'")
	}
}

func parseAssign(rest string) (node, *Error) {
	i := strings.Index(rest, "=")
	if i < 0 {
		return nil, parseErr("malformed assign tag: %q", rest)
	}
	target := strings.TrimSpace(rest[:i])
	if target == "" || !isIdentifier(target) {
		return nil, parseErr("malformed assign target: %q", target)
	}
	oe, err := parseOutputExpr(rest[i+1:])
	if err != nil {
		return nil, err
	}
	return &assignNode{target: target, expr: oe}, nil
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// expression scanner

type exprToken struct {
	kind string // ident, string, number, op, punct
	text string
	val  interface{}
}

func scanExpr(src string) ([]exprToken, *Error) {
	var out []exprToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '"' || c == '\'':
			quote := c
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != quote {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, parseErr("unterminated string literal")
			}
			out = append(out, exprToken{kind: "string", text: sb.String(), val: sb.String()})
			i = j + 1
		case c >= '0' && c <= '9', c == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9':
			j := i + 1
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				j++
			}
			out = append(out, exprToken{kind: "number", text: src[i:j], val: json.Number(src[i:j])})
			i = j
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i
			for j < len(src) {
				d := src[j]
				if d == '_' || d == '-' || (d >= 'a' && d <= 'z') || (d >= 'A' && d <= 'Z') || (d >= '0' && d <= '9') {
					j++
					continue
				}
				break
			}
			out = append(out, exprToken{kind: "ident", text: src[i:j]})
			i = j
		case c == '=' && i+1 < len(src) && src[i+1] == '=':
			out = append(out, exprToken{kind: "op", text: "=="})
			i += 2
		case c == '!' && i+1 < len(src) && src[i+1] == '=':
			out = append(out, exprToken{kind: "op", text: "!="})
			i += 2
		case c == '<' && i+1 < len(src) && src[i+1] == '>':
			out = append(out, exprToken{kind: "op", text: "!="})
			i += 2
		case c == '>' && i+1 < len(src) && src[i+1] == '=':
			out = append(out, exprToken{kind: "op", text: ">="})
			i += 2
		case c == '<' && i+1 < len(src) && src[i+1] == '=':
			out = append(out, exprToken{kind: "op", text: "<="})
			i += 2
		case c == '>' || c == '<':
			out = append(out, exprToken{kind: "op", text: string(c)})
			i++
		case c == '.' || c == '[' || c == ']' || c == '|' || c == ':' || c == ',':
			out = append(out, exprToken{kind: "punct", text: string(c)})
			i++
		default:
			return nil, parseErr("unexpected character %q in expression", string(c))
		}
	}
	return out, nil
}

type exprParser struct {
	toks []exprToken
	pos  int
}

func (p *exprParser) peek() *exprToken {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func (p *exprParser) next() *exprToken {
	t := p.peek()
	if t != nil {
		p.pos++
	}
	return t
}

// parseCondition parses a boolean expression: comparisons joined by and/or,
// folded right-to-left the way Liquid does.
func parseCondition(src string) (*expr, *Error) {
	toks, err := scanExpr(src)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, parseErr("empty condition")
	}
	p := &exprParser{toks: toks}
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, parseErr("trailing tokens in condition %q", src)
	}
	return e, nil
}

func (p *exprParser) parseOr() (*expr, *Error) {
	lhs, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t != nil && t.kind == "ident" && t.text == "or" {
		p.next()
		rhs, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		return &expr{kind: exprOr, lhs: lhs, rhs: rhs}, nil
	}
	return lhs, nil
}

func (p *exprParser) parseAnd() (*expr, *Error) {
	lhs, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t != nil && t.kind == "ident" && t.text == "and" {
		p.next()
		rhs, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		return &expr{kind: exprAnd, lhs: lhs, rhs: rhs}, nil
	}
	return lhs, nil
}

func (p *exprParser) parseComparison() (*expr, *Error) {
	lhs, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t == nil {
		return lhs, nil
	}
	op := ""
	if t.kind == "op" {
		op = t.text
	} else if t.kind == "ident" && t.text == "contains" {
		op = "contains"
	}
	if op == "" {
		return lhs, nil
	}
	p.next()
	rhs, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	return &expr{kind: exprBinary, op: op, lhs: lhs, rhs: rhs}, nil
}

func (p *exprParser) parseOperand() (*expr, *Error) {
	t := p.next()
	if t == nil {
		return nil, parseErr("unexpected end of expression")
	}
	switch t.kind {
	case "string":
		return &expr{kind: exprLiteral, literal: t.val}, nil
	case "number":
		return &expr{kind: exprLiteral, literal: t.val}, nil
	case "ident":
		switch t.text {
		case "true":
			return &expr{kind: exprLiteral, literal: true}, nil
		case "false":
			return &expr{kind: exprLiteral, literal: false}, nil
		case "nil", "null":
			return &expr{kind: exprLiteral, literal: nil}, nil
		case "empty", "blank":
			return &expr{kind: exprLiteral, literal: emptyMarker{}}, nil
		}
		return p.parsePath(t.text)
	default:
		return nil, parseErr("unexpected token %q in expression", t.text)
	}
}

type emptyMarker struct{}

func (p *exprParser) parsePath(head string) (*expr, *Error) {
	e := &expr{kind: exprVar, path: []pathSeg{{name: head}}, name: head}
	for {
		t := p.peek()
		if t == nil || t.kind != "punct" {
			break
		}
		switch t.text {
		case ".":
			p.next()
			nt := p.next()
			if nt == nil || nt.kind != "ident" {
				return nil, parseErr("expected a property name after '.'")
			}
			e.path = append(e.path, pathSeg{name: nt.text})
			e.name += "." + nt.text
		case "[":
			p.next()
			it := p.next()
			if it == nil {
				return nil, parseErr("expected an index after '['")
			}
			var seg pathSeg
			switch it.kind {
			case "number":
				n, err := it.val.(json.Number).Int64()
				if err != nil {
					return nil, parseErr("index must be an integer")
				}
				seg = pathSeg{isIndex: true, index: int(n)}
				e.name += fmt.Sprintf("[%d]", n)
			case "string":
				seg = pathSeg{name: it.text}
				e.name += fmt.Sprintf("[%q]", it.text)
			case "ident":
				sub, err := p.parsePath(it.text)
				if err != nil {
					return nil, err
				}
				seg = pathSeg{dynamic: sub}
				e.name += "[" + sub.name + "]"
			default:
				return nil, parseErr("unsupported index expression")
			}
			ct := p.next()
			if ct == nil || ct.text != "]" {
				return nil, parseErr("expected ']'")
			}
			e.path = append(e.path, seg)
		default:
			return e, nil
		}
	}
	return e, nil
}

func parseOperandString(src string) (*expr, *Error) {
	toks, err := scanExpr(src)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, parseErr("empty expression")
	}
	p := &exprParser{toks: toks}
	e, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, parseErr("trailing tokens in expression %q", src)
	}
	return e, nil
}

// parseOutputExpr parses `value | filter: arg | filter`.
func parseOutputExpr(src string) (*outputExpr, *Error) {
	toks, err := scanExpr(src)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, parseErr("empty output expression")
	}
	p := &exprParser{toks: toks}
	base, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	oe := &outputExpr{base: base}
	for {
		t := p.peek()
		if t == nil {
			break
		}
		if t.kind != "punct" || t.text != "|" {
			return nil, parseErr("unexpected token %q in output expression", t.text)
		}
		p.next()
		nt := p.next()
		if nt == nil || nt.kind != "ident" {
			return nil, parseErr("expected a filter name after '|'")
		}
		f := filterCall{name: nt.text}
		if c := p.peek(); c != nil && c.kind == "punct" && c.text == ":" {
			p.next()
			for {
				arg, err := p.parseOperand()
				if err != nil {
					return nil, err
				}
				f.args = append(f.args, arg)
				c2 := p.peek()
				if c2 != nil && c2.kind == "punct" && c2.text == "," {
					p.next()
					continue
				}
				break
			}
		}
		oe.filters = append(oe.filters, f)
	}
	return oe, nil
}

// removeBlankText implements Liquid's blank-body rule: when every entry of a
// block body is blank — whitespace-only text, or a tag that renders nothing
// such as assign — the whitespace is dropped entirely. It is why
// `{% unless forloop.last %} {% endunless %}` emits no separator, and skipping
// it makes a template render differently from every other PromptOn SDK.
func removeBlankText(body []node) []node {
	for _, n := range body {
		if !blankNode(n) {
			return body
		}
	}
	out := body[:0:0]
	for _, n := range body {
		if _, isText := n.(*textNode); !isText {
			out = append(out, n)
		}
	}
	return out
}

func blankNode(n node) bool {
	switch x := n.(type) {
	case *textNode:
		return strings.TrimSpace(x.text) == ""
	case *assignNode:
		return true
	default:
		return false
	}
}
