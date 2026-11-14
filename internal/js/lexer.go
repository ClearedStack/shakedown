// Package js parses and executes deterministic pure-calculation JavaScript
// scripts using only the Go standard library.
package js

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokNumber
	tokBigInt
	tokString
	tokTemplateHead   // `...${
	tokTemplateMiddle // }...${
	tokTemplateTail   // }...`
	tokTemplateNoSub  // `...`
	tokRegex

	tokLParen   // (
	tokRParen   // )
	tokLBrace   // {
	tokRBrace   // }
	tokLBracket // [
	tokRBracket // ]
	tokSemi     // ;
	tokComma    // ,
	tokDot      // .
	tokOptChain // ?.
	tokColon    // :
	tokQuestion // ?
	tokNullish  // ??
	tokArrow    // =>

	tokAssign    // =
	tokPlusEq    // +=
	tokMinusEq   // -=
	tokStarEq    // *=
	tokSlashEq   // /=
	tokPercentEq // %=

	tokPlus     // +
	tokMinus    // -
	tokStar     // *
	tokStarStar // **
	tokSlash    // /
	tokPercent  // %
	tokInc      // ++
	tokDec      // --

	tokBang    // !
	tokTilde   // ~
	tokAmp     // &
	tokPipe    // |
	tokCaret   // ^
	tokLShift  // <<
	tokRShift  // >>
	tokURShift // >>>
	tokAndAnd  // &&
	tokOrOr    // ||

	tokEqEq    // ==
	tokNotEq   // !=
	tokEqEqEq  // ===
	tokNotEqEq // !==
	tokLt      // <
	tokLtEq    // <=
	tokGt      // >
	tokGtEq    // >=

	// Keywords
	tokVar
	tokLet
	tokConst
	tokFunction
	tokReturn
	tokIf
	tokElse
	tokFor
	tokWhile
	tokDo
	tokBreak
	tokContinue
	tokSwitch
	tokCase
	tokDefault
	tokTry
	tokCatch
	tokFinally
	tokThrow
	tokNew
	tokTypeof
	tokVoid
	tokDelete
	tokIn
	tokInstanceof
	tokTrue
	tokFalse
	tokNull
	tokThis
)

var keywords = map[string]tokenKind{
	"var":        tokVar,
	"let":        tokLet,
	"const":      tokConst,
	"function":   tokFunction,
	"return":     tokReturn,
	"if":         tokIf,
	"else":       tokElse,
	"for":        tokFor,
	"while":      tokWhile,
	"do":         tokDo,
	"break":      tokBreak,
	"continue":   tokContinue,
	"switch":     tokSwitch,
	"case":       tokCase,
	"default":    tokDefault,
	"try":        tokTry,
	"catch":      tokCatch,
	"finally":    tokFinally,
	"throw":      tokThrow,
	"new":        tokNew,
	"typeof":     tokTypeof,
	"void":       tokVoid,
	"delete":     tokDelete,
	"in":         tokIn,
	"instanceof": tokInstanceof,
	"true":       tokTrue,
	"false":      tokFalse,
	"null":       tokNull,
	"this":       tokThis,
}

// unsupportedKeywords fail closed rather than guessing semantics.
var unsupportedKeywords = map[string]bool{
	"class": true, "extends": true, "super": true,
	"async": true, "await": true, "yield": true,
	"import": true, "export": true, "with": true, "debugger": true,
}

type token struct {
	kind    tokenKind
	lit     string
	flags   string
	num     float64
	newline bool // true if a line terminator appeared before this token
	pos     int
}

type lexer struct {
	src        string
	pos        int
	prevKind   tokenKind
	hasPrev    bool
	tplBraces  []int // stack of brace depths inside ${ ... }
	braceDepth int
}

func tokenize(src string) ([]token, error) {
	l := &lexer{src: src}
	var out []token
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
		if tok.kind == tokEOF {
			break
		}
		l.prevKind = tok.kind
		l.hasPrev = true
	}
	return out, nil
}

func (l *lexer) canStartRegex() bool {
	if !l.hasPrev {
		return true
	}
	switch l.prevKind {
	case tokIdent, tokNumber, tokBigInt, tokString, tokTemplateNoSub, tokTemplateTail,
		tokRegex, tokTrue, tokFalse, tokNull, tokThis,
		tokRParen, tokRBracket, tokInc, tokDec:
		return false
	case tokRBrace:
		// Conservatively treat / after } as division only when inside an
		// expression; top-level / after block is handled well enough by true.
		return true
	default:
		return true
	}
}

func (l *lexer) next() (token, error) {
	newline := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\f' || c == '\v' {
			l.pos++
			continue
		}
		if c == '\n' || c == '\r' {
			newline = true
			l.pos++
			continue
		}
		if c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
			l.pos += 2
			for l.pos < len(l.src) && l.src[l.pos] != '\n' && l.src[l.pos] != '\r' {
				l.pos++
			}
			continue
		}
		if c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
			l.pos += 2
			closed := false
			for l.pos+1 < len(l.src) {
				if l.src[l.pos] == '\n' || l.src[l.pos] == '\r' {
					newline = true
				}
				if l.src[l.pos] == '*' && l.src[l.pos+1] == '/' {
					l.pos += 2
					closed = true
					break
				}
				l.pos++
			}
			if !closed {
				return token{}, fmt.Errorf("unterminated block comment")
			}
			continue
		}
		// Non-ASCII whitespace such as BOM or NBSP
		if c >= 0x80 {
			r, w := utf8.DecodeRuneInString(l.src[l.pos:])
			if unicode.IsSpace(r) || r == '\ufeff' {
				if r == '\u2028' || r == '\u2029' {
					newline = true
				}
				l.pos += w
				continue
			}
		}
		break
	}

	if l.pos >= len(l.src) {
		return token{kind: tokEOF, newline: newline, pos: l.pos}, nil
	}

	start := l.pos
	c := l.src[l.pos]

	// Template continuation when closing '}' matches a ${ ... }
	if c == '}' && len(l.tplBraces) > 0 && l.braceDepth == l.tplBraces[len(l.tplBraces)-1] {
		l.tplBraces = l.tplBraces[:len(l.tplBraces)-1]
		l.pos++
		return l.scanTemplateChunk(start, newline, false)
	}

	if c == '`' {
		l.pos++
		return l.scanTemplateChunk(start, newline, true)
	}

	if c == '"' || c == '\'' {
		s, err := l.scanString(c)
		if err != nil {
			return token{}, err
		}
		return token{kind: tokString, lit: s, newline: newline, pos: start}, nil
	}

	if (c >= '0' && c <= '9') || (c == '.' && l.pos+1 < len(l.src) && l.src[l.pos+1] >= '0' && l.src[l.pos+1] <= '9') {
		return l.scanNumericToken(start, newline)
	}

	if isIdentStartByte(c) || c >= 0x80 {
		id, err := l.scanIdent()
		if err != nil {
			return token{}, err
		}
		if l.prevKind != tokDot && l.prevKind != tokOptChain {
			if unsupportedKeywords[id] {
				return token{}, fmt.Errorf("%w: keyword %s", ErrUnsupported, id)
			}
			if kw, ok := keywords[id]; ok {
				return token{kind: kw, lit: id, newline: newline, pos: start}, nil
			}
		}
		return token{kind: tokIdent, lit: id, newline: newline, pos: start}, nil
	}

	if c == '/' && l.canStartRegex() {
		// Check for /= only when not regex; in regex position /= is a regex starting with =
		pat, flags, err := l.scanRegex()
		if err != nil {
			return token{}, err
		}
		return token{kind: tokRegex, lit: pat, flags: flags, newline: newline, pos: start}, nil
	}

	// Punctuation and operators
	rem := l.src[l.pos:]
	switch {
	case strings.HasPrefix(rem, ">>>"):
		l.pos += 3
		return token{kind: tokURShift, lit: ">>>", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "==="):
		l.pos += 3
		return token{kind: tokEqEqEq, lit: "===", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "!=="):
		l.pos += 3
		return token{kind: tokNotEqEq, lit: "!==", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "=>"):
		l.pos += 2
		return token{kind: tokArrow, lit: "=>", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "=="):
		l.pos += 2
		return token{kind: tokEqEq, lit: "==", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "!="):
		l.pos += 2
		return token{kind: tokNotEq, lit: "!=", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "<="):
		l.pos += 2
		return token{kind: tokLtEq, lit: "<=", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, ">="):
		l.pos += 2
		return token{kind: tokGtEq, lit: ">=", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "<<"):
		l.pos += 2
		return token{kind: tokLShift, lit: "<<", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, ">>"):
		l.pos += 2
		return token{kind: tokRShift, lit: ">>", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "&&"):
		l.pos += 2
		return token{kind: tokAndAnd, lit: "&&", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "||"):
		l.pos += 2
		return token{kind: tokOrOr, lit: "||", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "??"):
		l.pos += 2
		return token{kind: tokNullish, lit: "??", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "?.") && (len(rem) < 3 || rem[2] < '0' || rem[2] > '9'):
		l.pos += 2
		return token{kind: tokOptChain, lit: "?.", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "++"):
		l.pos += 2
		return token{kind: tokInc, lit: "++", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "--"):
		l.pos += 2
		return token{kind: tokDec, lit: "--", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "**"):
		l.pos += 2
		return token{kind: tokStarStar, lit: "**", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "+="):
		l.pos += 2
		return token{kind: tokPlusEq, lit: "+=", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "-="):
		l.pos += 2
		return token{kind: tokMinusEq, lit: "-=", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "*="):
		l.pos += 2
		return token{kind: tokStarEq, lit: "*=", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "/="):
		l.pos += 2
		return token{kind: tokSlashEq, lit: "/=", newline: newline, pos: start}, nil
	case strings.HasPrefix(rem, "%="):
		l.pos += 2
		return token{kind: tokPercentEq, lit: "%=", newline: newline, pos: start}, nil
	}

	l.pos++
	switch c {
	case '(':
		return token{kind: tokLParen, lit: "(", newline: newline, pos: start}, nil
	case ')':
		return token{kind: tokRParen, lit: ")", newline: newline, pos: start}, nil
	case '{':
		l.braceDepth++
		return token{kind: tokLBrace, lit: "{", newline: newline, pos: start}, nil
	case '}':
		l.braceDepth--
		return token{kind: tokRBrace, lit: "}", newline: newline, pos: start}, nil
	case '[':
		return token{kind: tokLBracket, lit: "[", newline: newline, pos: start}, nil
	case ']':
		return token{kind: tokRBracket, lit: "]", newline: newline, pos: start}, nil
	case ';':
		return token{kind: tokSemi, lit: ";", newline: newline, pos: start}, nil
	case ',':
		return token{kind: tokComma, lit: ",", newline: newline, pos: start}, nil
	case '.':
		return token{kind: tokDot, lit: ".", newline: newline, pos: start}, nil
	case ':':
		return token{kind: tokColon, lit: ":", newline: newline, pos: start}, nil
	case '?':
		return token{kind: tokQuestion, lit: "?", newline: newline, pos: start}, nil
	case '=':
		return token{kind: tokAssign, lit: "=", newline: newline, pos: start}, nil
	case '+':
		return token{kind: tokPlus, lit: "+", newline: newline, pos: start}, nil
	case '-':
		return token{kind: tokMinus, lit: "-", newline: newline, pos: start}, nil
	case '*':
		return token{kind: tokStar, lit: "*", newline: newline, pos: start}, nil
	case '/':
		return token{kind: tokSlash, lit: "/", newline: newline, pos: start}, nil
	case '%':
		return token{kind: tokPercent, lit: "%", newline: newline, pos: start}, nil
	case '!':
		return token{kind: tokBang, lit: "!", newline: newline, pos: start}, nil
	case '~':
		return token{kind: tokTilde, lit: "~", newline: newline, pos: start}, nil
	case '&':
		return token{kind: tokAmp, lit: "&", newline: newline, pos: start}, nil
	case '|':
		return token{kind: tokPipe, lit: "|", newline: newline, pos: start}, nil
	case '^':
		return token{kind: tokCaret, lit: "^", newline: newline, pos: start}, nil
	case '<':
		return token{kind: tokLt, lit: "<", newline: newline, pos: start}, nil
	case '>':
		return token{kind: tokGt, lit: ">", newline: newline, pos: start}, nil
	}

	return token{}, fmt.Errorf("unexpected character")
}

func isIdentStartByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$'
}

func isIdentPartByte(c byte) bool {
	return isIdentStartByte(c) || (c >= '0' && c <= '9')
}

func (l *lexer) scanIdent() (string, error) {
	start := l.pos
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c < 0x80 {
			if isIdentPartByte(c) {
				l.pos++
				continue
			}
			break
		}
		r, w := utf8.DecodeRuneInString(l.src[l.pos:])
		if r == utf8.RuneError && w == 1 {
			return "", fmt.Errorf("invalid utf-8 in identifier")
		}
		if l.pos == start {
			if !unicode.IsLetter(r) {
				return "", fmt.Errorf("unexpected character")
			}
		} else if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			break
		}
		l.pos += w
	}
	if l.pos == start {
		return "", fmt.Errorf("unexpected character")
	}
	return l.src[start:l.pos], nil
}

func (l *lexer) scanNumericToken(start int, newline bool) (token, error) {
	if l.src[l.pos] == '0' && l.pos+1 < len(l.src) && (l.src[l.pos+1] == 'x' || l.src[l.pos+1] == 'X') {
		l.pos += 2
		hexStart := l.pos
		for l.pos < len(l.src) && isHexDigit(l.src[l.pos]) {
			l.pos++
		}
		if l.pos == hexStart {
			return token{}, fmt.Errorf("invalid hex literal")
		}
		if l.pos < len(l.src) && l.src[l.pos] == 'n' {
			raw := l.src[start:l.pos]
			l.pos++
			if l.pos < len(l.src) && isIdentPartByte(l.src[l.pos]) {
				return token{}, fmt.Errorf("invalid identifier immediately after bigint")
			}
			return token{kind: tokBigInt, lit: raw, newline: newline, pos: start}, nil
		}
		u, err := strconv.ParseUint(l.src[hexStart:l.pos], 16, 64)
		if err != nil {
			return token{}, err
		}
		if l.pos < len(l.src) && isIdentStartByte(l.src[l.pos]) {
			return token{}, fmt.Errorf("invalid identifier immediately after number")
		}
		return token{kind: tokNumber, lit: l.src[start:l.pos], num: float64(u), newline: newline, pos: start}, nil
	}

	hasDot := false
	hasExp := false
	for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		hasDot = true
		l.pos++
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		hasExp = true
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		expStart := l.pos
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
		if l.pos == expStart {
			return token{}, fmt.Errorf("invalid exponent")
		}
	}
	if !hasDot && !hasExp && l.pos < len(l.src) && l.src[l.pos] == 'n' {
		raw := l.src[start:l.pos]
		if len(raw) > 1 && raw[0] == '0' {
			return token{}, fmt.Errorf("invalid leading zero in bigint literal")
		}
		l.pos++
		if l.pos < len(l.src) && isIdentPartByte(l.src[l.pos]) {
			return token{}, fmt.Errorf("invalid identifier immediately after bigint")
		}
		return token{kind: tokBigInt, lit: raw, newline: newline, pos: start}, nil
	}
	if l.pos < len(l.src) && isIdentStartByte(l.src[l.pos]) {
		return token{}, fmt.Errorf("invalid identifier immediately after number")
	}
	raw := l.src[start:l.pos]
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return token{}, err
	}
	return token{kind: tokNumber, lit: raw, num: f, newline: newline, pos: start}, nil
}

func (l *lexer) scanString(quote byte) (string, error) {
	l.pos++ // consume opening quote
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == quote {
			l.pos++
			return b.String(), nil
		}
		if c == '\n' || c == '\r' {
			return "", fmt.Errorf("unterminated string literal")
		}
		if c == '\\' {
			l.pos++
			r, err := l.scanEscape()
			if err != nil {
				return "", err
			}
			b.WriteString(r)
			continue
		}
		b.WriteByte(c)
		l.pos++
	}
	return "", fmt.Errorf("unterminated string literal")
}

func (l *lexer) scanTemplateChunk(start int, newline bool, isFirst bool) (token, error) {
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '`' {
			l.pos++
			kind := tokTemplateTail
			if isFirst {
				kind = tokTemplateNoSub
			}
			return token{kind: kind, lit: b.String(), newline: newline, pos: start}, nil
		}
		if c == '$' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '{' {
			l.pos += 2
			l.tplBraces = append(l.tplBraces, l.braceDepth)
			kind := tokTemplateMiddle
			if isFirst {
				kind = tokTemplateHead
			}
			return token{kind: kind, lit: b.String(), newline: newline, pos: start}, nil
		}
		if c == '\\' {
			l.pos++
			r, err := l.scanEscape()
			if err != nil {
				return token{}, err
			}
			b.WriteString(r)
			continue
		}
		if c == '\r' {
			l.pos++
			if l.pos < len(l.src) && l.src[l.pos] == '\n' {
				l.pos++
			}
			b.WriteByte('\n')
			continue
		}
		b.WriteByte(c)
		l.pos++
	}
	return token{}, fmt.Errorf("unterminated template literal")
}

func (l *lexer) scanEscape() (string, error) {
	if l.pos >= len(l.src) {
		return "", fmt.Errorf("unterminated escape")
	}
	c := l.src[l.pos]
	l.pos++
	switch c {
	case 'n':
		return "\n", nil
	case 'r':
		return "\r", nil
	case 't':
		return "\t", nil
	case 'b':
		return "\b", nil
	case 'f':
		return "\f", nil
	case 'v':
		return "\v", nil
	case '0':
		return "\x00", nil
	case '\\', '\'', '"', '`', '$':
		return string(c), nil
	case '\n':
		return "", nil
	case '\r':
		if l.pos < len(l.src) && l.src[l.pos] == '\n' {
			l.pos++
		}
		return "", nil
	case 'x':
		if l.pos+2 > len(l.src) {
			return "", fmt.Errorf("invalid hex escape")
		}
		u, err := strconv.ParseUint(l.src[l.pos:l.pos+2], 16, 32)
		if err != nil {
			return "", err
		}
		l.pos += 2
		return string(rune(u)), nil
	case 'u':
		if l.pos < len(l.src) && l.src[l.pos] == '{' {
			l.pos++
			end := strings.IndexByte(l.src[l.pos:], '}')
			if end <= 0 {
				return "", fmt.Errorf("invalid unicode escape")
			}
			u, err := strconv.ParseUint(l.src[l.pos:l.pos+end], 16, 32)
			if err != nil || u > 0x10ffff {
				return "", fmt.Errorf("invalid unicode code point")
			}
			l.pos += end + 1
			return string(rune(u)), nil
		}
		if l.pos+4 > len(l.src) {
			return "", fmt.Errorf("invalid unicode escape")
		}
		u, err := strconv.ParseUint(l.src[l.pos:l.pos+4], 16, 32)
		if err != nil {
			return "", err
		}
		l.pos += 4
		return string(rune(u)), nil
	default:
		return string(c), nil
	}
}

func (l *lexer) scanRegex() (string, string, error) {
	l.pos++ // consume '/'
	start := l.pos
	inClass := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\n' || c == '\r' {
			return "", "", fmt.Errorf("unterminated regex literal")
		}
		if c == '\\' {
			l.pos += 2
			if l.pos > len(l.src) {
				return "", "", fmt.Errorf("unterminated regex escape")
			}
			continue
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if c == '/' && !inClass {
			pat := l.src[start:l.pos]
			l.pos++
			flagStart := l.pos
			for l.pos < len(l.src) && isIdentPartByte(l.src[l.pos]) {
				l.pos++
			}
			return pat, l.src[flagStart:l.pos], nil
		}
		l.pos++
	}
	return "", "", fmt.Errorf("unterminated regex literal")
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
