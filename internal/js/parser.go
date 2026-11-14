package js

import (
	"errors"
	"fmt"
)

// ErrSyntax indicates a JavaScript syntax error.
var ErrSyntax = errors.New("js syntax error")

// ErrUnsupported indicates valid-looking JavaScript outside the deterministic
// subset this interpreter covers.
var ErrUnsupported = errors.New("js construct not supported")

const maxParseDepth = 256

// Program is a parsed JavaScript file ready for static scanning or execution.
type Program struct {
	Name        string
	Identifiers []string
	Strings     []string
	body        []stmt
	hoistedVars []string
	hoistedFns  []*funcDecl
}

// Parse parses src into a Program, recording all identifiers and string or
// template literals encountered.
func Parse(name, src string) (*Program, error) {
	tokens, err := tokenize(src)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrSyntax, err)
	}
	p := &parser{
		tokens: tokens,
		prog:   &Program{Name: name},
	}
	stmts, err := p.parseStmtList(tokEOF)
	if err != nil {
		return nil, err
	}
	p.prog.body = stmts
	p.prog.hoistedVars, p.prog.hoistedFns = collectHoists(stmts)
	return p.prog, nil
}

// AST nodes

type stmt interface {
	stmtNode()
}

type blockStmt struct{ stmts []stmt }
type emptyStmt struct{}
type exprStmt struct{ expr expr }
type varDeclStmt struct {
	kind  tokenKind // tokVar, tokLet, tokConst
	decls []varBinding
}
type varBinding struct {
	name string
	init expr
}
type funcDecl struct {
	name        string
	params      []string
	body        []stmt
	hoistedVars []string
	hoistedFns  []*funcDecl
}
type returnStmt struct{ value expr }
type ifStmt struct {
	cond     expr
	thenStmt stmt
	elseStmt stmt
}
type whileStmt struct {
	cond expr
	body stmt
}
type doWhileStmt struct {
	body stmt
	cond expr
}
type forStmt struct {
	init stmt // varDeclStmt, exprStmt, or nil
	cond expr
	post expr
	body stmt
}
type forInOfStmt struct {
	isOf    bool
	varKind tokenKind // 0 if existing variable
	name    string
	rhs     expr
	body    stmt
}
type breakStmt struct{}
type continueStmt struct{}
type throwStmt struct{ value expr }
type switchStmt struct {
	discriminant expr
	cases        []switchCase
}
type switchCase struct {
	test  expr // nil for default
	stmts []stmt
}
type tryStmt struct {
	block       []stmt
	catchName   string
	hasCatch    bool
	catchBody   []stmt
	hasFinally  bool
	finallyBody []stmt
}

func (*blockStmt) stmtNode()    {}
func (*emptyStmt) stmtNode()    {}
func (*exprStmt) stmtNode()     {}
func (*varDeclStmt) stmtNode()  {}
func (*funcDecl) stmtNode()     {}
func (*returnStmt) stmtNode()   {}
func (*ifStmt) stmtNode()       {}
func (*whileStmt) stmtNode()    {}
func (*doWhileStmt) stmtNode()  {}
func (*forStmt) stmtNode()      {}
func (*forInOfStmt) stmtNode()  {}
func (*breakStmt) stmtNode()    {}
func (*continueStmt) stmtNode() {}
func (*throwStmt) stmtNode()    {}
func (*switchStmt) stmtNode()   {}
func (*tryStmt) stmtNode()      {}

type expr interface {
	exprNode()
}

type identExpr struct{ name string }
type literalExpr struct {
	kind  tokenKind // tokNumber, tokString, tokTrue, tokFalse, tokNull
	str   string
	num   float64
	regex *regexLit
}
type regexLit struct {
	pattern string
	flags   string
}
type thisExpr struct{}
type arrayExpr struct{ elems []expr }
type objectExpr struct{ props []propInit }
type propInit struct {
	key      string
	computed expr
	value    expr
}
type templateExpr struct {
	quasis []string
	exprs  []expr
}
type funcExpr struct {
	name        string
	params      []string
	body        []stmt
	hoistedVars []string
	hoistedFns  []*funcDecl
}
type memberExpr struct {
	object   expr
	prop     string
	index    expr // non-nil for computed obj[index]
	optional bool
}
type callExpr struct {
	callee   expr
	args     []expr
	isNew    bool
	optional bool
}
type unaryExpr struct {
	op  tokenKind
	arg expr
}
type updateExpr struct {
	op     tokenKind // tokInc or tokDec
	prefix bool
	arg    expr
}
type binaryExpr struct {
	op    tokenKind
	left  expr
	right expr
}
type assignExpr struct {
	op    tokenKind
	left  expr
	right expr
}
type condExpr struct {
	cond expr
	cons expr
	alt  expr
}
type seqExpr struct{ exprs []expr }

func (*identExpr) exprNode()    {}
func (*literalExpr) exprNode()  {}
func (*thisExpr) exprNode()     {}
func (*arrayExpr) exprNode()    {}
func (*objectExpr) exprNode()   {}
func (*templateExpr) exprNode() {}
func (*funcExpr) exprNode()     {}
func (*memberExpr) exprNode()   {}
func (*callExpr) exprNode()     {}
func (*unaryExpr) exprNode()    {}
func (*updateExpr) exprNode()   {}
func (*binaryExpr) exprNode()   {}
func (*assignExpr) exprNode()   {}
func (*condExpr) exprNode()     {}
func (*seqExpr) exprNode()      {}

type parser struct {
	tokens   []token
	pos      int
	depth    int
	fnDepth  int
	loopSw   int
	noIn     bool
	prog     *Program
}

func (p *parser) cur() token {
	if p.pos >= len(p.tokens) {
		return token{kind: tokEOF}
	}
	return p.tokens[p.pos]
}

func (p *parser) peek(offset int) token {
	idx := p.pos + offset
	if idx >= len(p.tokens) {
		return token{kind: tokEOF}
	}
	return p.tokens[idx]
}

func (p *parser) next() token {
	t := p.cur()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return t
}

func (p *parser) expect(k tokenKind) (token, error) {
	t := p.cur()
	if t.kind != k {
		return token{}, fmt.Errorf("%w: unexpected token", ErrSyntax)
	}
	p.pos++
	return t, nil
}

func (p *parser) consumeSemi() error {
	t := p.cur()
	if t.kind == tokSemi {
		p.pos++
		return nil
	}
	if t.kind == tokEOF || t.kind == tokRBrace || t.newline {
		return nil
	}
	return fmt.Errorf("%w: missing semicolon", ErrSyntax)
}

func (p *parser) noteIdent(name string) {
	if name != "" {
		p.prog.Identifiers = append(p.prog.Identifiers, name)
	}
}

func (p *parser) noteString(s string) {
	p.prog.Strings = append(p.prog.Strings, s)
}

func (p *parser) enter() error {
	p.depth++
	if p.depth > maxParseDepth {
		return fmt.Errorf("%w: nesting too deep", ErrSyntax)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) parseStmtList(stop tokenKind) ([]stmt, error) {
	var out []stmt
	for p.cur().kind != stop && p.cur().kind != tokEOF {
		st, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	if stop != tokEOF {
		if _, err := p.expect(stop); err != nil {
			return nil, err
		}
	} else if p.cur().kind != tokEOF {
		return nil, fmt.Errorf("%w: unexpected trailing tokens", ErrSyntax)
	}
	return out, nil
}

func (p *parser) parseStmt() (stmt, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	switch p.cur().kind {
	case tokSemi:
		p.next()
		return &emptyStmt{}, nil
	case tokLBrace:
		p.next()
		stmts, err := p.parseStmtList(tokRBrace)
		if err != nil {
			return nil, err
		}
		return &blockStmt{stmts: stmts}, nil
	case tokVar, tokLet, tokConst:
		decl, err := p.parseVarDecl(false)
		if err != nil {
			return nil, err
		}
		if err := p.consumeSemi(); err != nil {
			return nil, err
		}
		return decl, nil
	case tokFunction:
		return p.parseFuncDecl()
	case tokReturn:
		if p.fnDepth == 0 {
			return nil, fmt.Errorf("%w: return outside function", ErrSyntax)
		}
		p.next()
		if p.cur().kind == tokSemi || p.cur().kind == tokRBrace || p.cur().kind == tokEOF || p.cur().newline {
			_ = p.consumeSemi()
			return &returnStmt{}, nil
		}
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.consumeSemi(); err != nil {
			return nil, err
		}
		return &returnStmt{value: val}, nil
	case tokIf:
		return p.parseIfStmt()
	case tokWhile:
		return p.parseWhileStmt()
	case tokDo:
		return p.parseDoWhileStmt()
	case tokFor:
		return p.parseForStmt()
	case tokBreak:
		if p.loopSw == 0 {
			return nil, fmt.Errorf("%w: break outside loop or switch", ErrSyntax)
		}
		p.next()
		if p.cur().kind == tokIdent && !p.cur().newline {
			return nil, fmt.Errorf("%w: labeled break", ErrUnsupported)
		}
		if err := p.consumeSemi(); err != nil {
			return nil, err
		}
		return &breakStmt{}, nil
	case tokContinue:
		if p.loopSw == 0 {
			return nil, fmt.Errorf("%w: continue outside loop", ErrSyntax)
		}
		p.next()
		if p.cur().kind == tokIdent && !p.cur().newline {
			return nil, fmt.Errorf("%w: labeled continue", ErrUnsupported)
		}
		if err := p.consumeSemi(); err != nil {
			return nil, err
		}
		return &continueStmt{}, nil
	case tokThrow:
		p.next()
		if p.cur().newline || p.cur().kind == tokSemi || p.cur().kind == tokEOF {
			return nil, fmt.Errorf("%w: newline or missing expression after throw", ErrSyntax)
		}
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.consumeSemi(); err != nil {
			return nil, err
		}
		return &throwStmt{value: val}, nil
	case tokSwitch:
		return p.parseSwitchStmt()
	case tokTry:
		return p.parseTryStmt()
	default:
		if p.cur().kind == tokIdent && p.peek(1).kind == tokColon {
			return nil, fmt.Errorf("%w: labeled statement", ErrUnsupported)
		}
		ex, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.consumeSemi(); err != nil {
			return nil, err
		}
		return &exprStmt{expr: ex}, nil
	}
}

func (p *parser) parseVarDecl(noIn bool) (*varDeclStmt, error) {
	kind := p.next().kind
	oldNoIn := p.noIn
	p.noIn = noIn
	defer func() { p.noIn = oldNoIn }()

	var decls []varBinding
	for {
		id, err := p.expect(tokIdent)
		if err != nil {
			return nil, err
		}
		p.noteIdent(id.lit)
		var init expr
		if p.cur().kind == tokAssign {
			p.next()
			init, err = p.parseAssignExpr()
			if err != nil {
				return nil, err
			}
		} else if kind == tokConst && !noIn {
			return nil, fmt.Errorf("%w: const requires initializer", ErrSyntax)
		}
		decls = append(decls, varBinding{name: id.lit, init: init})
		if p.cur().kind != tokComma {
			break
		}
		p.next()
	}
	return &varDeclStmt{kind: kind, decls: decls}, nil
}

func (p *parser) parseFuncDecl() (*funcDecl, error) {
	if _, err := p.expect(tokFunction); err != nil {
		return nil, err
	}
	id, err := p.expect(tokIdent)
	if err != nil {
		return nil, err
	}
	p.noteIdent(id.lit)
	params, body, err := p.parseFuncParamsAndBody()
	if err != nil {
		return nil, err
	}
	vars, fns := collectHoists(body)
	return &funcDecl{name: id.lit, params: params, body: body, hoistedVars: vars, hoistedFns: fns}, nil
}

func (p *parser) parseFuncParamsAndBody() ([]string, []stmt, error) {
	if _, err := p.expect(tokLParen); err != nil {
		return nil, nil, err
	}
	var params []string
	for p.cur().kind != tokRParen {
		id, err := p.expect(tokIdent)
		if err != nil {
			return nil, nil, err
		}
		p.noteIdent(id.lit)
		params = append(params, id.lit)
		if p.cur().kind == tokComma {
			p.next()
			continue
		}
		break
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, nil, err
	}
	if _, err := p.expect(tokLBrace); err != nil {
		return nil, nil, err
	}
	p.fnDepth++
	oldLoop := p.loopSw
	p.loopSw = 0
	body, err := p.parseStmtList(tokRBrace)
	p.loopSw = oldLoop
	p.fnDepth--
	if err != nil {
		return nil, nil, err
	}
	return params, body, nil
}

func (p *parser) parseIfStmt() (stmt, error) {
	p.next() // if
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	thenSt, err := p.parseStmt()
	if err != nil {
		return nil, err
	}
	var elseSt stmt
	if p.cur().kind == tokElse {
		p.next()
		elseSt, err = p.parseStmt()
		if err != nil {
			return nil, err
		}
	}
	return &ifStmt{cond: cond, thenStmt: thenSt, elseStmt: elseSt}, nil
}

func (p *parser) parseWhileStmt() (stmt, error) {
	p.next() // while
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	p.loopSw++
	body, err := p.parseStmt()
	p.loopSw--
	if err != nil {
		return nil, err
	}
	return &whileStmt{cond: cond, body: body}, nil
}

func (p *parser) parseDoWhileStmt() (stmt, error) {
	p.next() // do
	p.loopSw++
	body, err := p.parseStmt()
	p.loopSw--
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokWhile); err != nil {
		return nil, err
	}
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	if p.cur().kind == tokSemi {
		p.next()
	}
	return &doWhileStmt{body: body, cond: cond}, nil
}

func (p *parser) parseForStmt() (stmt, error) {
	p.next() // for
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}

	var initStmt stmt
	if p.cur().kind != tokSemi {
		if p.cur().kind == tokVar || p.cur().kind == tokLet || p.cur().kind == tokConst {
			vd, err := p.parseVarDecl(true)
			if err != nil {
				return nil, err
			}
			if len(vd.decls) == 1 && vd.decls[0].init == nil && (p.cur().kind == tokIn || (p.cur().kind == tokIdent && p.cur().lit == "of")) {
				isOf := p.cur().kind == tokIdent && p.cur().lit == "of"
				p.next()
				rhs, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				if _, err := p.expect(tokRParen); err != nil {
					return nil, err
				}
				p.loopSw++
				body, err := p.parseStmt()
				p.loopSw--
				if err != nil {
					return nil, err
				}
				return &forInOfStmt{isOf: isOf, varKind: vd.kind, name: vd.decls[0].name, rhs: rhs, body: body}, nil
			}
			initStmt = vd
		} else {
			oldNoIn := p.noIn
			p.noIn = true
			ex, err := p.parseExpr()
			p.noIn = oldNoIn
			if err != nil {
				return nil, err
			}
			if id, ok := ex.(*identExpr); ok && (p.cur().kind == tokIn || (p.cur().kind == tokIdent && p.cur().lit == "of")) {
				isOf := p.cur().kind == tokIdent && p.cur().lit == "of"
				p.next()
				rhs, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				if _, err := p.expect(tokRParen); err != nil {
					return nil, err
				}
				p.loopSw++
				body, err := p.parseStmt()
				p.loopSw--
				if err != nil {
					return nil, err
				}
				return &forInOfStmt{isOf: isOf, name: id.name, rhs: rhs, body: body}, nil
			}
			initStmt = &exprStmt{expr: ex}
		}
	}
	if _, err := p.expect(tokSemi); err != nil {
		return nil, err
	}
	var cond expr
	if p.cur().kind != tokSemi {
		var err error
		cond, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(tokSemi); err != nil {
		return nil, err
	}
	var post expr
	if p.cur().kind != tokRParen {
		var err error
		post, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	p.loopSw++
	body, err := p.parseStmt()
	p.loopSw--
	if err != nil {
		return nil, err
	}
	return &forStmt{init: initStmt, cond: cond, post: post, body: body}, nil
}

func (p *parser) parseSwitchStmt() (stmt, error) {
	p.next() // switch
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	disc, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	if _, err := p.expect(tokLBrace); err != nil {
		return nil, err
	}
	p.loopSw++
	defer func() { p.loopSw-- }()

	var cases []switchCase
	seenDefault := false
	for p.cur().kind != tokRBrace && p.cur().kind != tokEOF {
		var sc switchCase
		switch p.cur().kind {
		case tokCase:
			p.next()
			t, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			sc.test = t
		case tokDefault:
			if seenDefault {
				return nil, fmt.Errorf("%w: multiple default clauses", ErrSyntax)
			}
			seenDefault = true
			p.next()
		default:
			return nil, fmt.Errorf("%w: expected case or default", ErrSyntax)
		}
		if _, err := p.expect(tokColon); err != nil {
			return nil, err
		}
		for p.cur().kind != tokCase && p.cur().kind != tokDefault && p.cur().kind != tokRBrace && p.cur().kind != tokEOF {
			st, err := p.parseStmt()
			if err != nil {
				return nil, err
			}
			sc.stmts = append(sc.stmts, st)
		}
		cases = append(cases, sc)
	}
	if _, err := p.expect(tokRBrace); err != nil {
		return nil, err
	}
	return &switchStmt{discriminant: disc, cases: cases}, nil
}

func (p *parser) parseTryStmt() (stmt, error) {
	p.next() // try
	if _, err := p.expect(tokLBrace); err != nil {
		return nil, err
	}
	block, err := p.parseStmtList(tokRBrace)
	if err != nil {
		return nil, err
	}
	ts := &tryStmt{block: block}
	if p.cur().kind == tokCatch {
		p.next()
		ts.hasCatch = true
		if p.cur().kind == tokLParen {
			p.next()
			id, err := p.expect(tokIdent)
			if err != nil {
				return nil, err
			}
			p.noteIdent(id.lit)
			ts.catchName = id.lit
			if _, err := p.expect(tokRParen); err != nil {
				return nil, err
			}
		}
		if _, err := p.expect(tokLBrace); err != nil {
			return nil, err
		}
		ts.catchBody, err = p.parseStmtList(tokRBrace)
		if err != nil {
			return nil, err
		}
	}
	if p.cur().kind == tokFinally {
		p.next()
		ts.hasFinally = true
		if _, err := p.expect(tokLBrace); err != nil {
			return nil, err
		}
		ts.finallyBody, err = p.parseStmtList(tokRBrace)
		if err != nil {
			return nil, err
		}
	}
	if !ts.hasCatch && !ts.hasFinally {
		return nil, fmt.Errorf("%w: try without catch or finally", ErrSyntax)
	}
	return ts, nil
}

// Expressions

func (p *parser) parseExpr() (expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	first, err := p.parseAssignExpr()
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tokComma {
		return first, nil
	}
	exprs := []expr{first}
	for p.cur().kind == tokComma {
		p.next()
		nx, err := p.parseAssignExpr()
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, nx)
	}
	return &seqExpr{exprs: exprs}, nil
}

func (p *parser) parseAssignExpr() (expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	// Single-param arrow function: x => ...
	if p.cur().kind == tokIdent && p.peek(1).kind == tokArrow && !p.peek(1).newline {
		id := p.next()
		p.noteIdent(id.lit)
		p.next() // =>
		return p.finishArrow([]string{id.lit})
	}

	// Multi-param or zero-param arrow function: (a, b) => ...
	if p.cur().kind == tokLParen {
		if params, ok := p.tryArrowParams(); ok {
			return p.finishArrow(params)
		}
	}

	left, err := p.parseCondExpr()
	if err != nil {
		return nil, err
	}
	switch p.cur().kind {
	case tokAssign, tokPlusEq, tokMinusEq, tokStarEq, tokSlashEq, tokPercentEq:
		if !isValidLValue(left) {
			return nil, fmt.Errorf("%w: invalid assignment target", ErrSyntax)
		}
		op := p.next().kind
		right, err := p.parseAssignExpr()
		if err != nil {
			return nil, err
		}
		return &assignExpr{op: op, left: left, right: right}, nil
	}
	return left, nil
}

func (p *parser) tryArrowParams() ([]string, bool) {
	saved := p.pos
	p.next() // (
	var params []string
	for p.cur().kind != tokRParen {
		if p.cur().kind != tokIdent {
			p.pos = saved
			return nil, false
		}
		params = append(params, p.next().lit)
		if p.cur().kind == tokComma {
			p.next()
			continue
		}
		break
	}
	if p.cur().kind != tokRParen {
		p.pos = saved
		return nil, false
	}
	p.next() // )
	if p.cur().kind != tokArrow || p.cur().newline {
		p.pos = saved
		return nil, false
	}
	p.next() // =>
	for _, name := range params {
		p.noteIdent(name)
	}
	return params, true
}

func (p *parser) finishArrow(params []string) (expr, error) {
	var body []stmt
	if p.cur().kind == tokLBrace {
		p.next()
		p.fnDepth++
		oldLoop := p.loopSw
		p.loopSw = 0
		var err error
		body, err = p.parseStmtList(tokRBrace)
		p.loopSw = oldLoop
		p.fnDepth--
		if err != nil {
			return nil, err
		}
	} else {
		p.fnDepth++
		ex, err := p.parseAssignExpr()
		p.fnDepth--
		if err != nil {
			return nil, err
		}
		body = []stmt{&returnStmt{value: ex}}
	}
	vars, fns := collectHoists(body)
	return &funcExpr{params: params, body: body, hoistedVars: vars, hoistedFns: fns}, nil
}

func isValidLValue(e expr) bool {
	switch v := e.(type) {
	case *identExpr:
		return true
	case *memberExpr:
		return !v.optional
	default:
		return false
	}
}

func (p *parser) parseCondExpr() (expr, error) {
	cond, err := p.parseBinaryExpr(1)
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tokQuestion {
		return cond, nil
	}
	p.next()
	oldNoIn := p.noIn
	p.noIn = false
	cons, err := p.parseAssignExpr()
	p.noIn = oldNoIn
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokColon); err != nil {
		return nil, err
	}
	alt, err := p.parseAssignExpr()
	if err != nil {
		return nil, err
	}
	return &condExpr{cond: cond, cons: cons, alt: alt}, nil
}

func (p *parser) binPrec(k tokenKind) int {
	switch k {
	case tokNullish:
		return 1
	case tokOrOr:
		return 2
	case tokAndAnd:
		return 3
	case tokPipe:
		return 4
	case tokCaret:
		return 5
	case tokAmp:
		return 6
	case tokEqEq, tokNotEq, tokEqEqEq, tokNotEqEq:
		return 7
	case tokLt, tokLtEq, tokGt, tokGtEq, tokInstanceof:
		return 8
	case tokIn:
		if p.noIn {
			return 0
		}
		return 8
	case tokLShift, tokRShift, tokURShift:
		return 9
	case tokPlus, tokMinus:
		return 10
	case tokStar, tokSlash, tokPercent:
		return 11
	case tokStarStar:
		return 12
	default:
		return 0
	}
}

func (p *parser) parseBinaryExpr(minPrec int) (expr, error) {
	left, err := p.parseUnaryExpr()
	if err != nil {
		return nil, err
	}
	for {
		prec := p.binPrec(p.cur().kind)
		if prec < minPrec {
			break
		}
		op := p.next().kind
		nextMin := prec + 1
		if op == tokStarStar {
			nextMin = prec
		}
		right, err := p.parseBinaryExpr(nextMin)
		if err != nil {
			return nil, err
		}
		left = &binaryExpr{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseUnaryExpr() (expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	switch p.cur().kind {
	case tokBang, tokTilde, tokPlus, tokMinus, tokTypeof, tokVoid, tokDelete:
		op := p.next().kind
		arg, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		return &unaryExpr{op: op, arg: arg}, nil
	case tokInc, tokDec:
		op := p.next().kind
		arg, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		if !isValidLValue(arg) {
			return nil, fmt.Errorf("%w: invalid update target", ErrSyntax)
		}
		return &updateExpr{op: op, prefix: true, arg: arg}, nil
	default:
		return p.parsePostfixExpr()
	}
}

func (p *parser) parsePostfixExpr() (expr, error) {
	ex, err := p.parseCallOrMemberExpr()
	if err != nil {
		return nil, err
	}
	if !p.cur().newline && (p.cur().kind == tokInc || p.cur().kind == tokDec) {
		if !isValidLValue(ex) {
			return nil, fmt.Errorf("%w: invalid postfix target", ErrSyntax)
		}
		op := p.next().kind
		return &updateExpr{op: op, prefix: false, arg: ex}, nil
	}
	return ex, nil
}

func (p *parser) parseCallOrMemberExpr() (expr, error) {
	var base expr
	var err error
	if p.cur().kind == tokNew {
		p.next()
		callee, err := p.parseMemberChain()
		if err != nil {
			return nil, err
		}
		var args []expr
		if p.cur().kind == tokLParen {
			args, err = p.parseArgList()
			if err != nil {
				return nil, err
			}
		}
		base = &callExpr{callee: callee, args: args, isNew: true}
	} else {
		base, err = p.parsePrimaryExpr()
		if err != nil {
			return nil, err
		}
	}
	return p.finishSubscripts(base, true)
}

func (p *parser) parseMemberChain() (expr, error) {
	var base expr
	var err error
	if p.cur().kind == tokNew {
		p.next()
		callee, err := p.parseMemberChain()
		if err != nil {
			return nil, err
		}
		var args []expr
		if p.cur().kind == tokLParen {
			args, err = p.parseArgList()
			if err != nil {
				return nil, err
			}
		}
		base = &callExpr{callee: callee, args: args, isNew: true}
	} else {
		base, err = p.parsePrimaryExpr()
		if err != nil {
			return nil, err
		}
	}
	return p.finishSubscripts(base, false)
}

func (p *parser) finishSubscripts(base expr, allowCalls bool) (expr, error) {
	for {
		switch p.cur().kind {
		case tokDot:
			p.next()
			id, err := p.expect(tokIdent)
			if err != nil {
				return nil, err
			}
			p.noteIdent(id.lit)
			base = &memberExpr{object: base, prop: id.lit}
		case tokOptChain:
			p.next()
			if p.cur().kind == tokLBracket {
				p.next()
				oldNoIn := p.noIn
				p.noIn = false
				idx, err := p.parseExpr()
				p.noIn = oldNoIn
				if err != nil {
					return nil, err
				}
				if _, err := p.expect(tokRBracket); err != nil {
					return nil, err
				}
				base = &memberExpr{object: base, index: idx, optional: true}
			} else if allowCalls && p.cur().kind == tokLParen {
				args, err := p.parseArgList()
				if err != nil {
					return nil, err
				}
				base = &callExpr{callee: base, args: args, optional: true}
			} else {
				id, err := p.expect(tokIdent)
				if err != nil {
					return nil, err
				}
				p.noteIdent(id.lit)
				base = &memberExpr{object: base, prop: id.lit, optional: true}
			}
		case tokLBracket:
			p.next()
			oldNoIn := p.noIn
			p.noIn = false
			idx, err := p.parseExpr()
			p.noIn = oldNoIn
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokRBracket); err != nil {
				return nil, err
			}
			base = &memberExpr{object: base, index: idx}
		case tokLParen:
			if !allowCalls {
				return base, nil
			}
			args, err := p.parseArgList()
			if err != nil {
				return nil, err
			}
			base = &callExpr{callee: base, args: args}
		default:
			return base, nil
		}
	}
}

func (p *parser) parseArgList() ([]expr, error) {
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	oldNoIn := p.noIn
	p.noIn = false
	defer func() { p.noIn = oldNoIn }()

	var args []expr
	for p.cur().kind != tokRParen {
		ex, err := p.parseAssignExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, ex)
		if p.cur().kind == tokComma {
			p.next()
			if p.cur().kind == tokRParen {
				break
			}
			continue
		}
		break
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *parser) parsePrimaryExpr() (expr, error) {
	t := p.cur()
	switch t.kind {
	case tokIdent:
		p.next()
		p.noteIdent(t.lit)
		return &identExpr{name: t.lit}, nil
	case tokNumber:
		p.next()
		return &literalExpr{kind: tokNumber, num: t.num}, nil
	case tokBigInt:
		p.next()
		return &literalExpr{kind: tokBigInt, str: t.lit}, nil
	case tokString:
		p.next()
		p.noteString(t.lit)
		return &literalExpr{kind: tokString, str: t.lit}, nil
	case tokTrue, tokFalse, tokNull:
		p.next()
		return &literalExpr{kind: t.kind}, nil
	case tokThis:
		p.next()
		return &thisExpr{}, nil
	case tokRegex:
		p.next()
		return &literalExpr{kind: tokRegex, regex: &regexLit{pattern: t.lit, flags: t.flags}}, nil
	case tokTemplateNoSub:
		p.next()
		p.noteString(t.lit)
		return &templateExpr{quasis: []string{t.lit}}, nil
	case tokTemplateHead:
		return p.parseTemplateLiteral()
	case tokLParen:
		p.next()
		oldNoIn := p.noIn
		p.noIn = false
		ex, err := p.parseExpr()
		p.noIn = oldNoIn
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		return ex, nil
	case tokLBracket:
		return p.parseArrayLiteral()
	case tokLBrace:
		return p.parseObjectLiteral()
	case tokFunction:
		p.next()
		name := ""
		if p.cur().kind == tokIdent {
			id := p.next()
			name = id.lit
			p.noteIdent(name)
		}
		params, body, err := p.parseFuncParamsAndBody()
		if err != nil {
			return nil, err
		}
		vars, fns := collectHoists(body)
		return &funcExpr{name: name, params: params, body: body, hoistedVars: vars, hoistedFns: fns}, nil
	default:
		return nil, fmt.Errorf("%w: unexpected token in expression", ErrSyntax)
	}
}

func (p *parser) parseTemplateLiteral() (expr, error) {
	head := p.next()
	p.noteString(head.lit)
	quasis := []string{head.lit}
	var exprs []expr
	oldNoIn := p.noIn
	p.noIn = false
	defer func() { p.noIn = oldNoIn }()

	for {
		ex, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, ex)
		t := p.cur()
		switch t.kind {
		case tokTemplateMiddle:
			p.next()
			p.noteString(t.lit)
			quasis = append(quasis, t.lit)
		case tokTemplateTail:
			p.next()
			p.noteString(t.lit)
			quasis = append(quasis, t.lit)
			return &templateExpr{quasis: quasis, exprs: exprs}, nil
		default:
			return nil, fmt.Errorf("%w: unterminated template substitution", ErrSyntax)
		}
	}
}

func (p *parser) parseArrayLiteral() (expr, error) {
	p.next() // [
	oldNoIn := p.noIn
	p.noIn = false
	defer func() { p.noIn = oldNoIn }()

	var elems []expr
	for p.cur().kind != tokRBracket {
		if p.cur().kind == tokComma {
			p.next()
			elems = append(elems, &identExpr{name: "undefined"})
			continue
		}
		ex, err := p.parseAssignExpr()
		if err != nil {
			return nil, err
		}
		elems = append(elems, ex)
		if p.cur().kind == tokComma {
			p.next()
			continue
		}
		break
	}
	if _, err := p.expect(tokRBracket); err != nil {
		return nil, err
	}
	return &arrayExpr{elems: elems}, nil
}

func (p *parser) parseObjectLiteral() (expr, error) {
	p.next() // {
	oldNoIn := p.noIn
	p.noIn = false
	defer func() { p.noIn = oldNoIn }()

	var props []propInit
	for p.cur().kind != tokRBrace {
		var prop propInit
		switch p.cur().kind {
		case tokLBracket:
			p.next()
			comp, err := p.parseAssignExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokRBracket); err != nil {
				return nil, err
			}
			prop.computed = comp
			if _, err := p.expect(tokColon); err != nil {
				return nil, err
			}
			val, err := p.parseAssignExpr()
			if err != nil {
				return nil, err
			}
			prop.value = val
		case tokString:
			s := p.next()
			p.noteString(s.lit)
			prop.key = s.lit
			if _, err := p.expect(tokColon); err != nil {
				return nil, err
			}
			val, err := p.parseAssignExpr()
			if err != nil {
				return nil, err
			}
			prop.value = val
		case tokNumber:
			n := p.next()
			prop.key = n.lit
			if _, err := p.expect(tokColon); err != nil {
				return nil, err
			}
			val, err := p.parseAssignExpr()
			if err != nil {
				return nil, err
			}
			prop.value = val
		default:
			t := p.cur()
			if t.kind != tokIdent && keywords[t.lit] == 0 {
				return nil, fmt.Errorf("%w: invalid object property key", ErrSyntax)
			}
			p.next()
			if p.cur().kind == tokColon {
				p.next()
				val, err := p.parseAssignExpr()
				if err != nil {
					return nil, err
				}
				prop.key = t.lit
				prop.value = val
			} else if t.kind == tokIdent && (p.cur().kind == tokComma || p.cur().kind == tokRBrace) {
				// Shorthand { x }
				p.noteIdent(t.lit)
				prop.key = t.lit
				prop.value = &identExpr{name: t.lit}
			} else if p.cur().kind == tokLParen {
				// Method shorthand { fn(a) { ... } }
				params, body, err := p.parseFuncParamsAndBody()
				if err != nil {
					return nil, err
				}
				vars, fns := collectHoists(body)
				prop.key = t.lit
				prop.value = &funcExpr{name: t.lit, params: params, body: body, hoistedVars: vars, hoistedFns: fns}
			} else {
				return nil, fmt.Errorf("%w: expected ':' in object literal", ErrSyntax)
			}
		}
		props = append(props, prop)
		if p.cur().kind == tokComma {
			p.next()
			continue
		}
		break
	}
	if _, err := p.expect(tokRBrace); err != nil {
		return nil, err
	}
	return &objectExpr{props: props}, nil
}

func collectHoists(stmts []stmt) ([]string, []*funcDecl) {
	seenVar := map[string]bool{}
	var vars []string
	var fns []*funcDecl
	var walk func(s stmt)
	walk = func(s stmt) {
		if s == nil {
			return
		}
		switch v := s.(type) {
		case *funcDecl:
			fns = append(fns, v)
		case *varDeclStmt:
			if v.kind == tokVar {
				for _, d := range v.decls {
					if !seenVar[d.name] {
						seenVar[d.name] = true
						vars = append(vars, d.name)
					}
				}
			}
		case *blockStmt:
			for _, sub := range v.stmts {
				walk(sub)
			}
		case *ifStmt:
			walk(v.thenStmt)
			walk(v.elseStmt)
		case *whileStmt:
			walk(v.body)
		case *doWhileStmt:
			walk(v.body)
		case *forStmt:
			walk(v.init)
			walk(v.body)
		case *forInOfStmt:
			if v.varKind == tokVar && !seenVar[v.name] {
				seenVar[v.name] = true
				vars = append(vars, v.name)
			}
			walk(v.body)
		case *switchStmt:
			for _, sc := range v.cases {
				for _, sub := range sc.stmts {
					walk(sub)
				}
			}
		case *tryStmt:
			for _, sub := range v.block {
				walk(sub)
			}
			for _, sub := range v.catchBody {
				walk(sub)
			}
			for _, sub := range v.finallyBody {
				walk(sub)
			}
		}
	}
	for _, s := range stmts {
		walk(s)
	}
	return vars, fns
}
