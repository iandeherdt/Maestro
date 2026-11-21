// Package template renders prompt templates with a strict subset of Liquid
// (SPEC section 5.4).
//
// The subset is deliberate. There is no include or render tag, no assign or
// capture, and every filter is a pure function, so a template can only turn
// its inputs into text. Unknown variables, filters and tags are errors, at
// parse time where that is possible.
//
// Supported: {{ value | filter: arg }}, if / elsif / else / endif, unless,
// for / else / endfor with forloop, comment, raw, and the whitespace
// control markers {{- -}} and {%- -%}.
package template

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

const (
	// MaxOutputBytes bounds the rendered size.
	MaxOutputBytes = 1 << 20
	maxDepth       = 32
)

// Error is a parse or render failure. Line is 1-based.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("template line %d: %s", e.Line, e.Msg) }

func errf(line int, format string, a ...any) *Error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, a...)}
}

// Template is a parsed template. It is safe for concurrent use.
type Template struct{ nodes []node }

// Parse compiles src. Unknown tags and filters are reported here.
func Parse(src string) (*Template, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	nodes, end, err := p.block(0)
	if err != nil {
		return nil, err
	}
	if end != nil {
		return nil, errf(end.line, "unexpected tag %q", end.name)
	}
	return &Template{nodes: nodes}, nil
}

// Render executes the template. vars may contain nil, bool, int, int64,
// string, []any and map[string]any, nested freely.
func (t *Template) Render(vars map[string]any) (string, error) {
	r := &renderer{scopes: []map[string]any{vars}}
	if err := r.run(t.nodes); err != nil {
		return "", err
	}
	return r.out.String(), nil
}

// ---- lexer ----------------------------------------------------------------

type tokKind int

const (
	tText tokKind = iota
	tOutput
	tTag
)

type token struct {
	kind tokKind
	line int
	text string // literal text, or the inside of the delimiters
	name string // tag name
	rest string // tag arguments
}

func lex(src string) ([]token, error) {
	var toks []token
	line := 1
	trimNext := false
	for len(src) > 0 {
		i := strings.Index(src, "{")
		for i >= 0 && (i+1 >= len(src) || (src[i+1] != '{' && src[i+1] != '%')) {
			j := strings.Index(src[i+1:], "{")
			if j < 0 {
				i = -1
			} else {
				i += 1 + j
			}
		}
		text := src
		if i >= 0 {
			text = src[:i]
		}
		if trimNext {
			trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
			line += strings.Count(text[:len(text)-len(trimmed)], "\n")
			text = trimmed
			trimNext = false
		}
		startLine := line
		line += strings.Count(text, "\n")
		if i < 0 {
			if text != "" {
				toks = append(toks, token{kind: tText, line: startLine, text: text})
			}
			break
		}
		kind, closer := tOutput, "}}"
		if src[i+1] == '%' {
			kind, closer = tTag, "%}"
		}
		rest := src[i+2:]
		end := strings.Index(rest, closer)
		if end < 0 {
			return nil, errf(line, "unclosed %q", src[i:i+2])
		}
		inner := rest[:end]
		src = rest[end+2:]
		tagLine := line
		line += strings.Count(inner, "\n")
		if strings.HasPrefix(inner, "-") {
			inner = inner[1:]
			text = strings.TrimRightFunc(text, unicode.IsSpace)
		}
		if strings.HasSuffix(inner, "-") {
			inner = inner[:len(inner)-1]
			trimNext = true
		}
		if text != "" {
			toks = append(toks, token{kind: tText, line: startLine, text: text})
		}
		inner = strings.TrimSpace(inner)
		tok := token{kind: kind, line: tagLine, text: inner}
		if kind == tTag {
			tok.name, tok.rest, _ = strings.Cut(inner, " ")
			tok.rest = strings.TrimSpace(tok.rest)
			if tok.name == "raw" || tok.name == "comment" {
				body, after, ok := cutEndTag(src, "end"+tok.name)
				if !ok {
					return nil, errf(tagLine, "%s without end%s", tok.name, tok.name)
				}
				line += strings.Count(src[:len(src)-len(after)], "\n")
				src = after
				if tok.name == "raw" && body != "" {
					toks = append(toks, token{kind: tText, line: tagLine, text: body})
				}
				continue
			}
		}
		toks = append(toks, tok)
	}
	return toks, nil
}

// cutEndTag finds {% name %} (with optional trim markers) and returns the
// text before it and the text after it.
func cutEndTag(src, name string) (body, after string, ok bool) {
	off := 0
	for {
		i := strings.Index(src[off:], "{%")
		if i < 0 {
			return "", "", false
		}
		i += off
		j := strings.Index(src[i:], "%}")
		if j < 0 {
			return "", "", false
		}
		inner := strings.TrimSpace(strings.Trim(src[i+2:i+j], "-"))
		if inner == name {
			return src[:i], src[i+j+2:], true
		}
		off = i + 2
	}
}

// ---- parser ---------------------------------------------------------------

type node interface{}

type textNode struct{ text string }

type outputNode struct {
	line    int
	value   expr
	filters []filterCall
}

type filterCall struct {
	name string
	fn   filterFunc
	args []expr
}

type branch struct {
	cond   *cond
	negate bool
	body   []node
}

type ifNode struct {
	line     int
	branches []branch
	elseBody []node
}

type forNode struct {
	line     int
	name     string
	list     expr
	body     []node
	elseBody []node
}

type parser struct {
	toks []token
	pos  int
}

// block parses nodes until an end, else or elsif tag, which it returns.
func (p *parser) block(depth int) ([]node, *token, error) {
	if depth > maxDepth {
		return nil, nil, errf(p.line(), "tags are nested too deeply")
	}
	var nodes []node
	for p.pos < len(p.toks) {
		tok := p.toks[p.pos]
		p.pos++
		switch tok.kind {
		case tText:
			nodes = append(nodes, textNode{tok.text})
		case tOutput:
			n, err := parseOutput(tok)
			if err != nil {
				return nil, nil, err
			}
			nodes = append(nodes, n)
		case tTag:
			switch tok.name {
			case "if", "unless":
				n, err := p.parseIf(tok, depth)
				if err != nil {
					return nil, nil, err
				}
				nodes = append(nodes, n)
			case "for":
				n, err := p.parseFor(tok, depth)
				if err != nil {
					return nil, nil, err
				}
				nodes = append(nodes, n)
			case "elsif", "else", "endif", "endunless", "endfor":
				return nodes, &tok, nil
			default:
				return nil, nil, errf(tok.line, "unsupported tag %q", tok.name)
			}
		}
	}
	return nodes, nil, nil
}

func (p *parser) line() int {
	if p.pos < len(p.toks) {
		return p.toks[p.pos].line
	}
	if len(p.toks) > 0 {
		return p.toks[len(p.toks)-1].line
	}
	return 1
}

func (p *parser) parseIf(open token, depth int) (node, error) {
	n := &ifNode{line: open.line}
	endName := "end" + open.name
	cur := open
	for {
		c, err := parseCond(cur.rest, cur.line)
		if err != nil {
			return nil, err
		}
		body, end, err := p.block(depth + 1)
		if err != nil {
			return nil, err
		}
		n.branches = append(n.branches, branch{cond: c, negate: open.name == "unless" && cur.name == "unless", body: body})
		if end == nil {
			return nil, errf(open.line, "%s without %s", open.name, endName)
		}
		switch end.name {
		case "elsif":
			cur = *end
			continue
		case "else":
			if end.rest != "" {
				return nil, errf(end.line, "else takes no arguments")
			}
			body, end2, err := p.block(depth + 1)
			if err != nil {
				return nil, err
			}
			if end2 == nil || end2.name != endName {
				return nil, errf(open.line, "%s without %s", open.name, endName)
			}
			n.elseBody = body
			return n, nil
		case endName:
			return n, nil
		default:
			return nil, errf(end.line, "unexpected tag %q inside %s", end.name, open.name)
		}
	}
}

func (p *parser) parseFor(open token, depth int) (node, error) {
	sc := newScanner(open.rest, open.line)
	name, ok := sc.ident()
	if !ok || strings.Contains(name, ".") {
		return nil, errf(open.line, "for needs a loop variable name")
	}
	if kw, ok := sc.ident(); !ok || kw != "in" {
		return nil, errf(open.line, `for needs "in"`)
	}
	list, err := sc.value()
	if err != nil {
		return nil, err
	}
	if !sc.eof() {
		return nil, errf(open.line, "for options such as limit, offset and reversed are not supported")
	}
	n := &forNode{line: open.line, name: name, list: list}
	body, end, err := p.block(depth + 1)
	if err != nil {
		return nil, err
	}
	n.body = body
	if end != nil && end.name == "else" {
		n.elseBody, end, err = p.block(depth + 1)
		if err != nil {
			return nil, err
		}
	}
	if end == nil || end.name != "endfor" {
		return nil, errf(open.line, "for without endfor")
	}
	return n, nil
}

func parseOutput(tok token) (node, error) {
	sc := newScanner(tok.text, tok.line)
	v, err := sc.value()
	if err != nil {
		return nil, err
	}
	n := &outputNode{line: tok.line, value: v}
	for sc.eat("|") {
		name, ok := sc.ident()
		if !ok {
			return nil, errf(tok.line, "filter name expected")
		}
		f, known := filters[name]
		if !known {
			return nil, errf(tok.line, "unknown filter %q", name)
		}
		call := filterCall{name: name, fn: f.fn}
		if sc.eat(":") {
			for {
				a, err := sc.value()
				if err != nil {
					return nil, err
				}
				call.args = append(call.args, a)
				if !sc.eat(",") {
					break
				}
			}
		}
		if len(call.args) < f.min || len(call.args) > f.max {
			return nil, errf(tok.line, "filter %q takes %d to %d arguments", name, f.min, f.max)
		}
		n.filters = append(n.filters, call)
	}
	if !sc.eof() {
		return nil, errf(tok.line, "unexpected %q", sc.rest())
	}
	return n, nil
}

// ---- expressions ----------------------------------------------------------

type marker int

const (
	blankMarker marker = iota + 1
	emptyMarker
)

// expr is a literal or a variable path.
type expr struct {
	line    int
	literal any
	path    []any // string keys and int indexes; nil means literal
}

// cond is a comparison, optionally joined to the rest with and / or.
// Liquid evaluates these right to left without precedence.
type cond struct {
	left, right expr
	op          string // "" means truthiness of left
	join        string // "and", "or" or ""
	next        *cond
}

func parseCond(src string, line int) (*cond, error) {
	sc := newScanner(src, line)
	c, err := sc.cond()
	if err != nil {
		return nil, err
	}
	if !sc.eof() {
		return nil, errf(line, "unexpected %q", sc.rest())
	}
	return c, nil
}

type scanner struct {
	s    string
	pos  int
	line int
}

func newScanner(s string, line int) *scanner { return &scanner{s: s, line: line} }

func (s *scanner) skip() {
	for s.pos < len(s.s) && unicode.IsSpace(rune(s.s[s.pos])) {
		s.pos++
	}
}

func (s *scanner) eof() bool    { s.skip(); return s.pos >= len(s.s) }
func (s *scanner) rest() string { s.skip(); return s.s[s.pos:] }

func (s *scanner) eat(lit string) bool {
	s.skip()
	if strings.HasPrefix(s.s[s.pos:], lit) {
		s.pos += len(lit)
		return true
	}
	return false
}

func isIdentByte(c byte, first bool) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		!first && (c >= '0' && c <= '9' || c == '-')
}

// ident reads a bare word without consuming it on failure.
func (s *scanner) ident() (string, bool) {
	s.skip()
	start := s.pos
	for s.pos < len(s.s) && isIdentByte(s.s[s.pos], s.pos == start) {
		s.pos++
	}
	return s.s[start:s.pos], s.pos > start
}

func (s *scanner) value() (expr, error) {
	s.skip()
	e := expr{line: s.line}
	if s.pos >= len(s.s) {
		return e, errf(s.line, "value expected")
	}
	c := s.s[s.pos]
	switch {
	case c == '"' || c == '\'':
		end := strings.IndexByte(s.s[s.pos+1:], c)
		if end < 0 {
			return e, errf(s.line, "unclosed string")
		}
		e.literal = s.s[s.pos+1 : s.pos+1+end]
		s.pos += end + 2
		return e, nil
	case c >= '0' && c <= '9' || c == '-':
		start := s.pos
		s.pos++
		for s.pos < len(s.s) && s.s[s.pos] >= '0' && s.s[s.pos] <= '9' {
			s.pos++
		}
		n, err := strconv.ParseInt(s.s[start:s.pos], 10, 64)
		if err != nil {
			return e, errf(s.line, "bad number %q", s.s[start:s.pos])
		}
		e.literal = n
		return e, nil
	}
	name, ok := s.ident()
	if !ok {
		return e, errf(s.line, "unexpected %q", s.s[s.pos:])
	}
	switch name {
	case "true":
		e.literal = true
		return e, nil
	case "false":
		e.literal = false
		return e, nil
	case "nil", "null":
		e.path = []any{} // empty, non-nil path: evaluates to nil
		return e, nil
	case "blank":
		e.literal = blankMarker
		return e, nil
	case "empty":
		e.literal = emptyMarker
		return e, nil
	}
	e.path = []any{name}
	for s.pos < len(s.s) {
		if s.s[s.pos] == '.' {
			s.pos++
			start := s.pos
			for s.pos < len(s.s) && isIdentByte(s.s[s.pos], s.pos == start) {
				s.pos++
			}
			if s.pos == start {
				return e, errf(s.line, "name expected after '.'")
			}
			e.path = append(e.path, s.s[start:s.pos])
		} else if s.s[s.pos] == '[' {
			end := strings.IndexByte(s.s[s.pos:], ']')
			if end < 0 {
				return e, errf(s.line, "unclosed '['")
			}
			inner := strings.TrimSpace(s.s[s.pos+1 : s.pos+end])
			s.pos += end + 1
			if n, err := strconv.Atoi(inner); err == nil {
				e.path = append(e.path, n)
			} else if len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0] {
				e.path = append(e.path, inner[1:len(inner)-1])
			} else {
				return e, errf(s.line, "index must be a number or a quoted string")
			}
		} else {
			break
		}
	}
	return e, nil
}

var compareOps = []string{"==", "!=", "<>", "<=", ">=", "<", ">"}

func (s *scanner) cond() (*cond, error) {
	left, err := s.value()
	if err != nil {
		return nil, err
	}
	c := &cond{left: left}
	s.skip()
	for _, op := range compareOps {
		if s.eat(op) {
			c.op = op
			break
		}
	}
	save := s.pos
	if c.op == "" {
		if w, ok := s.ident(); ok && w == "contains" {
			c.op = w
		} else {
			s.pos = save
		}
	}
	if c.op != "" {
		if c.right, err = s.value(); err != nil {
			return nil, err
		}
	}
	save = s.pos
	if w, ok := s.ident(); ok && (w == "and" || w == "or") {
		c.join = w
		if c.next, err = s.cond(); err != nil {
			return nil, err
		}
	} else {
		s.pos = save
	}
	return c, nil
}

// ---- renderer -------------------------------------------------------------

type renderer struct {
	out    strings.Builder
	scopes []map[string]any
}

func (r *renderer) write(line int, s string) error {
	if r.out.Len()+len(s) > MaxOutputBytes {
		return errf(line, "output is larger than %d bytes", MaxOutputBytes)
	}
	r.out.WriteString(s)
	return nil
}

func (r *renderer) run(nodes []node) error {
	for _, n := range nodes {
		switch n := n.(type) {
		case textNode:
			if err := r.write(0, n.text); err != nil {
				return err
			}
		case *outputNode:
			v, err := r.eval(n.value)
			if err != nil {
				return err
			}
			for _, f := range n.filters {
				args := make([]any, len(f.args))
				for i, a := range f.args {
					if args[i], err = r.eval(a); err != nil {
						return err
					}
				}
				if v, err = f.fn(v, args); err != nil {
					return errf(n.line, "filter %q: %v", f.name, err)
				}
			}
			s, err := toText(v)
			if err != nil {
				return errf(n.line, "%v", err)
			}
			if err := r.write(n.line, s); err != nil {
				return err
			}
		case *ifNode:
			done := false
			for _, b := range n.branches {
				ok, err := r.test(b.cond)
				if err != nil {
					return err
				}
				if ok != b.negate {
					if err := r.run(b.body); err != nil {
						return err
					}
					done = true
					break
				}
			}
			if !done {
				if err := r.run(n.elseBody); err != nil {
					return err
				}
			}
		case *forNode:
			v, err := r.eval(n.list)
			if err != nil {
				return err
			}
			list, ok := v.([]any)
			if !ok && v != nil {
				return errf(n.line, "for needs a list")
			}
			if len(list) == 0 {
				if err := r.run(n.elseBody); err != nil {
					return err
				}
				continue
			}
			for i, item := range list {
				r.scopes = append(r.scopes, map[string]any{
					n.name: item,
					"forloop": map[string]any{
						"index": int64(i + 1), "index0": int64(i), "length": int64(len(list)),
						"first": i == 0, "last": i == len(list)-1,
					},
				})
				err := r.run(n.body)
				r.scopes = r.scopes[:len(r.scopes)-1]
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func pathString(path []any) string {
	var b strings.Builder
	for i, p := range path {
		switch p := p.(type) {
		case string:
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(p)
		case int:
			fmt.Fprintf(&b, "[%d]", p)
		}
	}
	return b.String()
}

func (r *renderer) eval(e expr) (any, error) {
	if e.path == nil {
		return e.literal, nil
	}
	if len(e.path) == 0 {
		return nil, nil
	}
	var cur any
	found := false
	root := e.path[0].(string)
	for i := len(r.scopes) - 1; i >= 0; i-- {
		if v, ok := r.scopes[i][root]; ok {
			cur, found = v, true
			break
		}
	}
	if !found {
		return nil, errf(e.line, "unknown variable %q", root)
	}
	for i, step := range e.path[1:] {
		unknown := func() (any, error) {
			return nil, errf(e.line, "unknown variable %q", pathString(e.path[:i+2]))
		}
		switch c := cur.(type) {
		case map[string]any:
			key, ok := step.(string)
			if !ok {
				return unknown()
			}
			v, ok := c[key]
			if !ok {
				return unknown()
			}
			cur = v
		case []any:
			switch step {
			case "size":
				cur = int64(len(c))
			case "first", "last":
				if len(c) == 0 {
					cur = nil
				} else if step == "first" {
					cur = c[0]
				} else {
					cur = c[len(c)-1]
				}
			default:
				idx, ok := step.(int)
				if !ok {
					return unknown()
				}
				if idx < 0 {
					idx += len(c)
				}
				if idx < 0 || idx >= len(c) {
					cur = nil
				} else {
					cur = c[idx]
				}
			}
		case string:
			if step != "size" {
				return unknown()
			}
			cur = int64(len([]rune(c)))
		default:
			return unknown()
		}
	}
	return normalize(cur), nil
}

func normalize(v any) any {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	}
	return v
}

func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	}
	return true
}

func (r *renderer) test(c *cond) (bool, error) {
	// Right to left: the tail is evaluated first, as Liquid does.
	var tail bool
	if c.next != nil {
		var err error
		if tail, err = r.test(c.next); err != nil {
			return false, err
		}
	}
	left, err := r.eval(c.left)
	if err != nil {
		return false, err
	}
	res := truthy(left)
	if c.op != "" {
		right, err := r.eval(c.right)
		if err != nil {
			return false, err
		}
		if res, err = compare(c.op, left, right); err != nil {
			return false, errf(c.left.line, "%v", err)
		}
	} else if _, isMarker := left.(marker); isMarker {
		return false, errf(c.left.line, "blank and empty can only be compared")
	}
	switch c.join {
	case "and":
		return res && tail, nil
	case "or":
		return res || tail, nil
	}
	return res, nil
}

func isBlank(v any, strict bool) bool {
	switch v := v.(type) {
	case nil:
		return !strict
	case bool:
		return !strict && !v
	case string:
		if strict {
			return v == ""
		}
		return strings.TrimSpace(v) == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

func compare(op string, a, b any) (bool, error) {
	if m, ok := b.(marker); ok {
		res := isBlank(a, m == emptyMarker)
		switch op {
		case "==":
			return res, nil
		case "!=", "<>":
			return !res, nil
		}
		return false, fmt.Errorf("blank and empty support only == and !=")
	}
	if _, ok := a.(marker); ok {
		return false, fmt.Errorf("blank and empty must be on the right-hand side")
	}
	switch op {
	case "==":
		return equal(a, b), nil
	case "!=", "<>":
		return !equal(a, b), nil
	case "contains":
		switch a := a.(type) {
		case string:
			s, ok := b.(string)
			return ok && strings.Contains(a, s), nil
		case []any:
			for _, item := range a {
				if equal(item, b) {
					return true, nil
				}
			}
			return false, nil
		case nil:
			return false, nil
		}
		return false, fmt.Errorf("contains needs a string or a list on the left")
	}
	x, ok1 := a.(int64)
	y, ok2 := b.(int64)
	if !ok1 || !ok2 {
		// Liquid treats an ordering comparison with nil as false.
		if a == nil || b == nil {
			return false, nil
		}
		return false, fmt.Errorf("%s compares numbers only", op)
	}
	switch op {
	case "<":
		return x < y, nil
	case ">":
		return x > y, nil
	case "<=":
		return x <= y, nil
	default:
		return x >= y, nil
	}
}

func equal(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && a == y
	case int64:
		y, ok := b.(int64)
		return ok && a == y
	case string:
		y, ok := b.(string)
		return ok && a == y
	}
	return false
}

func toText(v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "", nil
	case bool:
		return strconv.FormatBool(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case string:
		return v, nil
	case marker:
		return "", nil
	case []any:
		return "", fmt.Errorf("a list cannot be printed directly, use a for loop or the join filter")
	}
	return "", fmt.Errorf("an object cannot be printed directly, name one of its fields")
}

// ---- filters --------------------------------------------------------------

type filterFunc func(v any, args []any) (any, error)

type filterDef struct {
	fn       filterFunc
	min, max int
}

func str(v any) (string, error) {
	if _, isList := v.([]any); isList {
		return "", fmt.Errorf("expects text, got a list")
	}
	if _, isMap := v.(map[string]any); isMap {
		return "", fmt.Errorf("expects text, got an object")
	}
	return toText(v)
}

func stringFilter(f func(s string) string) filterFunc {
	return func(v any, _ []any) (any, error) {
		s, err := str(v)
		if err != nil {
			return nil, err
		}
		return f(s), nil
	}
}

func intArg(v any) (int64, error) {
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("expects a whole number")
	}
	return n, nil
}

// Every filter is a pure function of its inputs.
var filters = map[string]filterDef{
	"upcase":   {stringFilter(strings.ToUpper), 0, 0},
	"downcase": {stringFilter(strings.ToLower), 0, 0},
	"strip":    {stringFilter(strings.TrimSpace), 0, 0},
	"capitalize": {stringFilter(func(s string) string {
		r := []rune(s)
		if len(r) == 0 {
			return s
		}
		return string(unicode.ToUpper(r[0])) + string(r[1:])
	}), 0, 0},
	"default": {func(v any, a []any) (any, error) {
		if !truthy(v) || isBlank(v, true) {
			return a[0], nil
		}
		return v, nil
	}, 1, 1},
	"size": {func(v any, _ []any) (any, error) {
		switch v := v.(type) {
		case string:
			return int64(len([]rune(v))), nil
		case []any:
			return int64(len(v)), nil
		case nil:
			return int64(0), nil
		}
		return nil, fmt.Errorf("expects text or a list")
	}, 0, 0},
	"join": {func(v any, a []any) (any, error) {
		list, ok := v.([]any)
		if !ok && v != nil {
			return nil, fmt.Errorf("expects a list")
		}
		sep := " "
		if len(a) == 1 {
			s, err := str(a[0])
			if err != nil {
				return nil, err
			}
			sep = s
		}
		parts := make([]string, len(list))
		for i, item := range list {
			s, err := str(item)
			if err != nil {
				return nil, err
			}
			parts[i] = s
		}
		return strings.Join(parts, sep), nil
	}, 0, 1},
	"first": {func(v any, _ []any) (any, error) {
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("expects a list")
		}
		if len(list) == 0 {
			return nil, nil
		}
		return list[0], nil
	}, 0, 0},
	"last": {func(v any, _ []any) (any, error) {
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("expects a list")
		}
		if len(list) == 0 {
			return nil, nil
		}
		return list[len(list)-1], nil
	}, 0, 0},
	"truncate": {func(v any, a []any) (any, error) {
		s, err := str(v)
		if err != nil {
			return nil, err
		}
		n, err := intArg(a[0])
		if err != nil || n < 0 {
			return nil, fmt.Errorf("expects a length of 0 or more")
		}
		r := []rune(s)
		if int64(len(r)) <= n {
			return s, nil
		}
		const tail = "..."
		if n <= int64(len(tail)) {
			return tail[:n], nil
		}
		return string(r[:n-int64(len(tail))]) + tail, nil
	}, 1, 1},
	"replace": {func(v any, a []any) (any, error) {
		s, err := str(v)
		if err != nil {
			return nil, err
		}
		from, err1 := str(a[0])
		to, err2 := str(a[1])
		if err1 != nil || err2 != nil || from == "" {
			return nil, fmt.Errorf("expects two texts, the first not empty")
		}
		return strings.ReplaceAll(s, from, to), nil
	}, 2, 2},
	"append": {func(v any, a []any) (any, error) {
		s, err1 := str(v)
		t, err2 := str(a[0])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("expects text")
		}
		return s + t, nil
	}, 1, 1},
	"prepend": {func(v any, a []any) (any, error) {
		s, err1 := str(v)
		t, err2 := str(a[0])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("expects text")
		}
		return t + s, nil
	}, 1, 1},
	"plus": {func(v any, a []any) (any, error) {
		x, err1 := intArg(v)
		y, err2 := intArg(a[0])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("expects whole numbers")
		}
		return x + y, nil
	}, 1, 1},
	"minus": {func(v any, a []any) (any, error) {
		x, err1 := intArg(v)
		y, err2 := intArg(a[0])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("expects whole numbers")
		}
		return x - y, nil
	}, 1, 1},
}
