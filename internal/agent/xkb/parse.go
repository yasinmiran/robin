package xkb

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// token is a word, a quoted string (text without its quotes), a key name
// such as <AD01>, or one punctuation character.
type token struct {
	text   string
	quoted bool
}

// lex splits a symbols file into tokens, dropping comments.
func lex(src string) ([]token, error) {
	var toks []token
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '/' && strings.HasPrefix(src[i:], "//"), c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment")
			}
			i += end + 4
		case c == '"':
			end := strings.IndexByte(src[i+1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, token{text: src[i+1 : i+1+end], quoted: true})
			i += end + 2
		case c == '<':
			end := strings.IndexByte(src[i:], '>')
			if end < 0 {
				return nil, fmt.Errorf("unterminated key name")
			}
			toks = append(toks, token{text: src[i : i+end+1]})
			i += end + 1
		case unicode.IsSpace(rune(c)):
			i++
		case isWord(c):
			j := i
			for j < len(src) && isWord(src[j]) {
				j++
			}
			toks = append(toks, token{text: src[i:j]})
			i = j
		default:
			toks = append(toks, token{text: string(c)})
			i++
		}
	}
	return toks, nil
}

func isWord(c byte) bool {
	return c == '_' || c == '.' || c == '+' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// parser walks the tokens of one file.
type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return token{}
}

func (p *parser) next() token {
	t := p.peek()
	p.i++
	return t
}

func (p *parser) done() bool { return p.i >= len(p.toks) }

func (p *parser) expect(s string) error {
	if t := p.next(); t.text != s || t.quoted {
		return fmt.Errorf("want %q, got %q", s, t.text)
	}
	return nil
}

// parse reads the xkb_symbols sections of a symbols file.
func parse(src string) ([]section, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var secs []section
	for {
		sec, ok, err := p.header("xkb_symbols")
		if err != nil || !ok {
			return secs, err
		}
		if sec.stmts, err = p.body(); err != nil {
			return nil, fmt.Errorf("section %q: %w", sec.name, err)
		}
		p.endSection()
		secs = append(secs, sec)
	}
}

// parseKeycodes reads the default xkb_keycodes section of a keycodes file,
// or its first: the code of each key by its name or an alias of it.
func parseKeycodes(src string) (map[string]int, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var first map[string]int
	for {
		sec, ok, err := p.header("xkb_keycodes")
		if err != nil {
			return nil, err
		}
		if !ok {
			if first == nil {
				return nil, errors.New("no xkb_keycodes")
			}
			return first, nil
		}
		codes, err := p.keycodes()
		if err != nil {
			return nil, fmt.Errorf("section %q: %w", sec.name, err)
		}
		p.endSection()
		if sec.def {
			return codes, nil
		}
		if first == nil {
			first = codes
		}
	}
}

// header reads up to and including the opening brace of the next section
// of kind, and reports whether there was one.
func (p *parser) header(kind string) (section, bool, error) {
	var sec section
	for {
		if p.done() {
			return sec, false, nil
		}
		t := p.next()
		if t.quoted {
			continue
		}
		if t.text == "default" {
			sec.def = true
		}
		if t.text == kind {
			break
		}
	}
	name := p.next()
	if !name.quoted {
		return sec, false, fmt.Errorf("%s without a name", kind)
	}
	sec.name = name.text
	return sec, true, p.expect("{")
}

// endSection passes the semicolon after a section's closing brace.
func (p *parser) endSection() {
	if p.peek().text == ";" {
		p.next()
	}
}

// keycodes reads `<NAME> = 38;` and `alias <A> = <B>;` up to the section's
// closing brace, and gives each alias its key's code.
func (p *parser) keycodes() (map[string]int, error) {
	codes := map[string]int{}
	aliases := map[string]string{}
	for {
		if p.done() {
			return nil, errors.New("no closing brace")
		}
		t := p.next()
		switch {
		case t.text == ";":
			continue
		case t.text == "}" && !t.quoted:
			for alias, key := range aliases {
				if code, ok := codes[key]; ok {
					codes[alias] = code
				}
			}
			return codes, nil
		case t.text == "alias":
			alias := p.next()
			if p.next().text == "=" {
				aliases[alias.text] = p.next().text
			}
		case strings.HasPrefix(t.text, "<") && p.peek().text == "=":
			p.next()
			if code, err := strconv.Atoi(p.next().text); err == nil {
				codes[t.text] = code
			}
		}
		if err := p.skip(); err != nil {
			return nil, err
		}
	}
}

// body reads the statements of a section up to its closing brace.
func (p *parser) body() ([]stmt, error) {
	var stmts []stmt
	for {
		if p.done() {
			return nil, fmt.Errorf("no closing brace")
		}
		t := p.next()
		if t.text == "}" && !t.quoted {
			return stmts, nil
		}
		m := modeOverride
		switch t.text {
		case "augment":
			m = modeAugment
			fallthrough
		case "include", "override", "replace":
			if p.peek().quoted {
				stmts = append(stmts, stmt{include: p.next().text, mode: m})
				continue
			}
			t = p.next()
		}
		switch t.text {
		case "key":
			s, err := p.key(m)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, s)
		case "name":
			s, ok, err := p.groupName()
			if err != nil {
				return nil, err
			}
			if ok {
				stmts = append(stmts, s)
			}
		default:
			if err := p.skip(); err != nil {
				return nil, err
			}
		}
	}
}

// groupName reads `[Group1] = "Name";`; a name of another group is skipped.
func (p *parser) groupName() (stmt, bool, error) {
	var group string
	if p.peek().text == "[" {
		p.next()
		group = p.next().text
		if err := p.expect("]"); err != nil {
			return stmt{}, false, err
		}
	}
	if p.peek().text == "=" {
		p.next()
	}
	name := p.next()
	p.skip()
	if !name.quoted || !strings.EqualFold(group, "Group1") {
		return stmt{}, false, nil
	}
	return stmt{name: name.text}, true, nil
}

// key reads `<NAME> { ... };` for the first group's symbols.
func (p *parser) key(m mode) (stmt, error) {
	name := p.next()
	if !strings.HasPrefix(name.text, "<") {
		return stmt{}, fmt.Errorf("key without a name: %q", name.text)
	}
	if err := p.expect("{"); err != nil {
		return stmt{}, err
	}
	s := stmt{key: name.text, mode: m}
	group := 0 // bare lists so far
	for {
		t := p.next()
		switch {
		case p.i > len(p.toks):
			return stmt{}, fmt.Errorf("key %s: no closing brace", name.text)
		case t.quoted || t.text == ",":
		case t.text == "}":
			if p.peek().text == ";" {
				p.next()
			}
			return s, nil
		case t.text == "[":
			// A bare list is the next group's symbols.
			group++
			levels, err := p.list()
			if err != nil {
				return stmt{}, err
			}
			if group == 1 {
				s.levels = levels
			}
		default:
			// A field, as in type[Group1]="TWO_LEVEL", symbols[Group1]=[ q, Q ]
			// or actions[Group1]=[ SetMods(modifiers=Shift) ].
			var index string
			if p.peek().text == "[" {
				p.next()
				index = p.next().text
				if err := p.expect("]"); err != nil {
					return stmt{}, err
				}
			}
			if p.peek().text != "=" {
				continue
			}
			p.next()
			if p.peek().text != "[" {
				p.next()
				continue
			}
			p.next()
			levels, err := p.list()
			if err != nil {
				return stmt{}, err
			}
			if t.text == "symbols" && strings.EqualFold(index, "Group1") {
				s.levels = levels
			}
		}
	}
}

// list reads the levels of a key up to `]`, each keysym normalised; a
// level of several keysyms is written `{ a, b }`.
func (p *parser) list() ([][]string, error) {
	var levels [][]string
	several := false
	for {
		t := p.next()
		switch {
		case p.i > len(p.toks):
			return nil, fmt.Errorf("no closing bracket")
		case t.text == "]" && !t.quoted:
			return levels, nil
		case t.text == ",":
		case t.text == "{":
			levels = append(levels, nil)
			several = true
		case t.text == "}":
			several = false
		case t.text == "(":
			if err := p.skipTo(")"); err != nil {
				return nil, err
			}
		case several:
			levels[len(levels)-1] = append(levels[len(levels)-1], normalize(t.text))
		default:
			levels = append(levels, []string{normalize(t.text)})
		}
	}
}

// skip passes the rest of a statement, up to its semicolon at the same
// depth, or up to the end of the section without consuming its brace.
func (p *parser) skip() error {
	depth := 0
	for !p.done() {
		t := p.peek()
		if !t.quoted {
			switch t.text {
			case "{", "[", "(":
				depth++
			case "}", "]", ")":
				if depth == 0 {
					return nil
				}
				depth--
			case ";":
				if depth == 0 {
					p.next()
					return nil
				}
			}
		}
		p.next()
	}
	return nil
}

func (p *parser) skipTo(close string) error {
	for !p.done() {
		if t := p.next(); t.text == close && !t.quoted {
			return nil
		}
	}
	return fmt.Errorf("no %q", close)
}

// normalize names a keysym given by its code, U0061 or 0x61, by the name
// of the ASCII character it is, if it is one.
func normalize(sym string) string {
	var code uint64
	var err error
	switch {
	case len(sym) > 1 && (sym[0] == 'U' || sym[0] == 'u') && isHex(sym[1:]):
		code, err = strconv.ParseUint(sym[1:], 16, 32)
	case strings.HasPrefix(sym, "0x") || strings.HasPrefix(sym, "0X"):
		code, err = strconv.ParseUint(sym[2:], 16, 32)
		if code >= 0x1000000 {
			code -= 0x1000000
		}
	default:
		return sym
	}
	if err != nil || code < 0x21 || code > 0x7e {
		return sym
	}
	return asciiName(rune(code))
}

func isHex(s string) bool {
	for _, c := range s {
		if !unicode.Is(unicode.ASCII_Hex_Digit, c) {
			return false
		}
	}
	return s != ""
}
