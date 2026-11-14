package js

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrInterrupted is returned when script execution reaches its context deadline.
var ErrInterrupted = errors.New("js execution interrupted")

// ErrRuntime is returned when a script throws an uncaught runtime exception.
var ErrRuntime = errors.New("js runtime error")

// ErrNoFunction is returned when the named entry function is not defined or is
// not callable.
var ErrNoFunction = errors.New("js entry function not defined")

// Deterministic resource bounds keeping every VM well inside the 24 MiB Go
// heap limit and 30 MiB container ceiling.
const (
	maxCallStack   = 512
	maxStringBytes = 256 << 10
	maxArrayLen    = 16384
	maxObjectProps = 4096
	maxBigIntBits  = 16384
	maxVMAllocs    = 65536
	maxJSONDepth   = 128
)

var fixedClock = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

type valueKind int

const (
	kindUndefined valueKind = iota
	kindNull
	kindBool
	kindNumber
	kindBigInt
	kindString
	kindObject
	kindArray
	kindMap
	kindFunction
	kindRegex
)

type value struct {
	kind valueKind
	b    bool
	n    float64
	bi   *big.Int
	s    string
	obj  *jsObject
	arr  *jsArray
	mp   *jsMap
	fn   *jsFunc
	rx   *jsRegex
}

var (
	valUndefined = value{kind: kindUndefined}
	valNull      = value{kind: kindNull}
	valTrue      = value{kind: kindBool, b: true}
	valFalse     = value{kind: kindBool, b: false}
	valNaN       = value{kind: kindNumber, n: math.NaN()}
)

func boolVal(b bool) value {
	if b {
		return valTrue
	}
	return valFalse
}

func numVal(n float64) value   { return value{kind: kindNumber, n: n} }
func strVal(s string) value    { return value{kind: kindString, s: s} }
func objVal(o *jsObject) value { return value{kind: kindObject, obj: o} }
func arrVal(a *jsArray) value  { return value{kind: kindArray, arr: a} }
func mapVal(m *jsMap) value    { return value{kind: kindMap, mp: m} }
func fnVal(f *jsFunc) value    { return value{kind: kindFunction, fn: f} }

type jsObject struct {
	props map[string]value
	order []string
}

func newObject() *jsObject {
	return &jsObject{props: make(map[string]value)}
}

func (o *jsObject) get(k string) (value, bool) {
	v, ok := o.props[k]
	return v, ok
}

func (o *jsObject) set(k string, v value) {
	if _, exists := o.props[k]; !exists {
		o.order = append(o.order, k)
	}
	o.props[k] = v
}

func (o *jsObject) del(k string) {
	if _, exists := o.props[k]; !exists {
		return
	}
	delete(o.props, k)
	for i, key := range o.order {
		if key == k {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
}

type jsArray struct {
	elems []value
}

type mapEntry struct {
	key    value
	val    value
	active bool
}

type jsMap struct {
	entries []mapEntry
	size    int
}

func sameValueZero(a, b value) bool {
	if a.kind == kindNumber && b.kind == kindNumber {
		if math.IsNaN(a.n) && math.IsNaN(b.n) {
			return true
		}
		return a.n == b.n
	}
	return strictEqual(a, b)
}

func (m *jsMap) findIndex(k value) int {
	for i := range m.entries {
		if m.entries[i].active && sameValueZero(m.entries[i].key, k) {
			return i
		}
	}
	return -1
}

func (m *jsMap) get(k value) (value, bool) {
	if i := m.findIndex(k); i >= 0 {
		return m.entries[i].val, true
	}
	return valUndefined, false
}

func (m *jsMap) set(vm *VM, k, v value) *flow {
	if i := m.findIndex(k); i >= 0 {
		m.entries[i].val = v
		return nil
	}
	if m.size >= maxObjectProps {
		return vm.throwError("RangeError", "maximum Map size exceeded")
	}
	if f := vm.chargeAlloc(1); f != nil {
		return f
	}
	if k.kind == kindNumber && k.n == 0 {
		k = numVal(0)
	}
	m.entries = append(m.entries, mapEntry{key: k, val: v, active: true})
	m.size++
	return nil
}

func (m *jsMap) del(k value) bool {
	if i := m.findIndex(k); i >= 0 {
		m.entries[i] = mapEntry{}
		m.size--
		return true
	}
	return false
}

func (m *jsMap) clear() {
	m.entries = nil
	m.size = 0
}

type nativeFn func(vm *VM, thisVal value, args []value) (value, *flow)

type jsFunc struct {
	name        string
	params      []string
	body        []stmt
	hoistedVars []string
	hoistedFns  []*funcDecl
	closure     *env
	native      nativeFn
	props       *jsObject
}

type jsRegex struct {
	pattern string
	flags   string
	re      *regexp.Regexp
}

type binding struct {
	val   value
	isCon bool
}

type env struct {
	parent *env
	vars   map[string]*binding
}

func newEnv(parent *env) *env {
	return &env{parent: parent, vars: make(map[string]*binding)}
}

func (e *env) declare(name string, val value, isConst bool) {
	if b, ok := e.vars[name]; ok {
		if !b.isCon {
			b.val = val
		}
		return
	}
	e.vars[name] = &binding{val: val, isCon: isConst}
}

func (e *env) lookup(name string) (*binding, bool) {
	for cur := e; cur != nil; cur = cur.parent {
		if b, ok := cur.vars[name]; ok {
			return b, true
		}
	}
	return nil, false
}

type flowKind int

const (
	flowNone flowKind = iota
	flowReturn
	flowBreak
	flowContinue
	flowThrow
	flowAbort // deadline or fatal VM limit
)

type flow struct {
	kind flowKind
	val  value
	err  error
}

// VM is a single isolated JavaScript execution environment.
type VM struct {
	global    *env
	globalObj *jsObject
	ctx       context.Context
	steps     uint64
	callDepth int
	allocs    int
	inNew     bool
}

// NewVM constructs a fresh isolated VM with deterministic standard built-ins.
func NewVM() *VM {
	vm := &VM{
		global:    newEnv(nil),
		globalObj: newObject(),
		ctx:       context.Background(),
	}
	vm.installBuiltins()
	vm.allocs = 0
	return vm
}

func (vm *VM) chargeAlloc(n int) *flow {
	if n <= 0 {
		n = 1
	}
	if vm.allocs+n > maxVMAllocs {
		return vm.throwError("RangeError", "maximum VM allocation budget exceeded")
	}
	vm.allocs += n
	return nil
}

func (vm *VM) makeString(s string) (value, *flow) {
	if len(s) > maxStringBytes {
		return valUndefined, vm.throwError("RangeError", "maximum string length exceeded")
	}
	if len(s) > 64 {
		if f := vm.chargeAlloc(1 + len(s)/4096); f != nil {
			return valUndefined, f
		}
	}
	return strVal(s), nil
}

func (vm *VM) makeArray(elems []value) (value, *flow) {
	if len(elems) > maxArrayLen {
		return valUndefined, vm.throwError("RangeError", "maximum array length exceeded")
	}
	if f := vm.chargeAlloc(1 + len(elems)/64); f != nil {
		return valUndefined, f
	}
	return arrVal(&jsArray{elems: elems}), nil
}

func (vm *VM) newCheckedObject() (*jsObject, *flow) {
	if f := vm.chargeAlloc(1); f != nil {
		return nil, f
	}
	return newObject(), nil
}

func (vm *VM) setObjProp(o *jsObject, k string, v value) *flow {
	if _, exists := o.props[k]; !exists && len(o.order) >= maxObjectProps {
		return vm.throwError("RangeError", "maximum object property count exceeded")
	}
	o.set(k, v)
	if o == vm.globalObj {
		vm.global.declare(k, v, false)
	}
	return nil
}

func (vm *VM) makeBigInt(z *big.Int) (value, *flow) {
	if z.BitLen() > maxBigIntBits {
		return valUndefined, vm.throwError("RangeError", "maximum BigInt size exceeded")
	}
	if f := vm.chargeAlloc(1); f != nil {
		return valUndefined, f
	}
	return value{kind: kindBigInt, bi: z}, nil
}

func (vm *VM) defGlobal(name string, v value) {
	vm.global.declare(name, v, false)
	vm.globalObj.set(name, v)
}

func makeNative(name string, fn nativeFn) value {
	return fnVal(&jsFunc{name: name, native: fn, props: newObject()})
}

func (vm *VM) throwTypeError(msg string) *flow {
	errObj := newObject()
	errObj.set("name", strVal("TypeError"))
	errObj.set("message", strVal(msg))
	return &flow{kind: flowThrow, val: objVal(errObj)}
}

func (vm *VM) throwError(name, msg string) *flow {
	errObj := newObject()
	errObj.set("name", strVal(name))
	errObj.set("message", strVal(msg))
	return &flow{kind: flowThrow, val: objVal(errObj)}
}

func (vm *VM) tick() *flow {
	vm.steps++
	if vm.steps&0xff == 0 {
		if err := vm.ctx.Err(); err != nil {
			return &flow{kind: flowAbort, err: ErrInterrupted}
		}
	}
	return nil
}

// RunProgram executes top-level statements of prog inside vm.
func (vm *VM) RunProgram(ctx context.Context, prog *Program) error {
	vm.ctx = ctx
	if err := ctx.Err(); err != nil {
		return ErrInterrupted
	}
	for _, name := range prog.hoistedVars {
		if _, exists := vm.global.vars[name]; !exists {
			vm.defGlobal(name, valUndefined)
		}
	}
	for _, fd := range prog.hoistedFns {
		fn := vm.instantiateFuncDecl(fd, vm.global)
		vm.defGlobal(fd.name, fn)
	}
	for _, st := range prog.body {
		if f := vm.execStmt(st, vm.global); f != nil {
			return vm.flowToError(f)
		}
	}
	return nil
}

// HasFunction reports whether name is a global function in vm.
func (vm *VM) HasFunction(name string) bool {
	b, ok := vm.global.lookup(name)
	return ok && b.val.kind == kindFunction
}

// CallJSON parses inputJSON as a JS value, invokes the global function fnName
// with that value, and returns JSON.stringify of its return value (or "" when
// the return value is undefined).
func (vm *VM) CallJSON(ctx context.Context, fnName string, inputJSON []byte) (string, error) {
	vm.ctx = ctx
	if err := ctx.Err(); err != nil {
		return "", ErrInterrupted
	}
	b, ok := vm.global.lookup(fnName)
	if !ok || b.val.kind != kindFunction {
		return "", ErrNoFunction
	}
	arg, err := vm.parseJSONValueChecked(inputJSON)
	if err != nil {
		return "", fmt.Errorf("%w: invalid input JSON", ErrRuntime)
	}
	res, f := vm.callFunction(b.val.fn, valUndefined, []value{arg})
	if f != nil {
		return "", vm.flowToError(f)
	}
	if res.kind == kindUndefined || res.kind == kindFunction {
		return "", nil
	}
	out, ok, f := stringifyValue(vm, res)
	if f != nil {
		return "", vm.flowToError(f)
	}
	if !ok {
		return "", nil
	}
	return out, nil
}

// EvalJSON parses and evaluates exprSrc inside vm and returns JSON.stringify of
// its resulting value (or "" when undefined).
func (vm *VM) EvalJSON(ctx context.Context, exprSrc string) (string, error) {
	vm.ctx = ctx
	if err := ctx.Err(); err != nil {
		return "", ErrInterrupted
	}
	prog, err := Parse("<eval>", "("+exprSrc+");")
	if err != nil {
		return "", err
	}
	if len(prog.body) != 1 {
		return "", ErrSyntax
	}
	es, ok := prog.body[0].(*exprStmt)
	if !ok {
		return "", ErrSyntax
	}
	res, f := vm.evalExpr(es.expr, vm.global)
	if f != nil {
		return "", vm.flowToError(f)
	}
	if res.kind == kindUndefined || res.kind == kindFunction {
		return "", nil
	}
	out, ok, f := stringifyValue(vm, res)
	if f != nil {
		return "", vm.flowToError(f)
	}
	if !ok {
		return "", nil
	}
	return out, nil
}

func (vm *VM) flowToError(f *flow) error {
	if f == nil {
		return nil
	}
	if f.kind == flowAbort && f.err != nil {
		return f.err
	}
	return ErrRuntime
}

func (vm *VM) instantiateFuncDecl(fd *funcDecl, scope *env) value {
	return fnVal(&jsFunc{
		name:        fd.name,
		params:      fd.params,
		body:        fd.body,
		hoistedVars: fd.hoistedVars,
		hoistedFns:  fd.hoistedFns,
		closure:     scope,
		props:       newObject(),
	})
}

func (vm *VM) callFunction(fn *jsFunc, thisVal value, args []value) (value, *flow) {
	if f := vm.tick(); f != nil {
		return valUndefined, f
	}
	vm.callDepth++
	if vm.callDepth > maxCallStack {
		vm.callDepth--
		return valUndefined, vm.throwError("RangeError", "maximum call stack size exceeded")
	}
	defer func() { vm.callDepth-- }()

	if fn.native != nil {
		return fn.native(vm, thisVal, args)
	}

	if f := vm.chargeAlloc(1); f != nil {
		return valUndefined, f
	}
	callEnv := newEnv(fn.closure)
	if fn.name != "" {
		callEnv.declare(fn.name, fnVal(fn), false)
	}
	callEnv.declare("this", thisVal, false)
	argElems := make([]value, len(args))
	copy(argElems, args)
	argArr, af := vm.makeArray(argElems)
	if af != nil {
		return valUndefined, af
	}
	callEnv.declare("arguments", argArr, false)

	for i, p := range fn.params {
		v := valUndefined
		if i < len(args) {
			v = args[i]
		}
		callEnv.declare(p, v, false)
	}
	for _, vname := range fn.hoistedVars {
		if _, exists := callEnv.vars[vname]; !exists {
			callEnv.declare(vname, valUndefined, false)
		}
	}
	for _, fd := range fn.hoistedFns {
		callEnv.declare(fd.name, vm.instantiateFuncDecl(fd, callEnv), false)
	}

	for _, st := range fn.body {
		f := vm.execStmt(st, callEnv)
		if f == nil {
			continue
		}
		if f.kind == flowReturn {
			return f.val, nil
		}
		return valUndefined, f
	}
	return valUndefined, nil
}

// Statement execution

func (vm *VM) execStmt(st stmt, scope *env) *flow {
	if f := vm.tick(); f != nil {
		return f
	}
	switch s := st.(type) {
	case nil, *emptyStmt, *funcDecl:
		return nil
	case *blockStmt:
		blockScope := newEnv(scope)
		for _, sub := range s.stmts {
			if f := vm.execStmt(sub, blockScope); f != nil {
				return f
			}
		}
		return nil
	case *exprStmt:
		_, f := vm.evalExpr(s.expr, scope)
		return f
	case *varDeclStmt:
		for _, d := range s.decls {
			val := valUndefined
			if d.init != nil {
				var f *flow
				val, f = vm.evalExpr(d.init, scope)
				if f != nil {
					return f
				}
			}
			if s.kind == tokVar {
				if b, ok := scope.lookup(d.name); ok {
					if b.isCon {
						return vm.throwTypeError("assignment to constant variable")
					}
					b.val = val
					if b == vm.global.vars[d.name] {
						vm.globalObj.set(d.name, val)
					}
				} else {
					vm.defGlobal(d.name, val)
				}
			} else {
				scope.declare(d.name, val, s.kind == tokConst)
				if scope == vm.global {
					vm.globalObj.set(d.name, val)
				}
			}
		}
		return nil
	case *returnStmt:
		if s.value == nil {
			return &flow{kind: flowReturn, val: valUndefined}
		}
		v, f := vm.evalExpr(s.value, scope)
		if f != nil {
			return f
		}
		return &flow{kind: flowReturn, val: v}
	case *ifStmt:
		c, f := vm.evalExpr(s.cond, scope)
		if f != nil {
			return f
		}
		if toBool(c) {
			return vm.execStmt(s.thenStmt, scope)
		}
		if s.elseStmt != nil {
			return vm.execStmt(s.elseStmt, scope)
		}
		return nil
	case *whileStmt:
		for {
			if f := vm.tick(); f != nil {
				return f
			}
			c, f := vm.evalExpr(s.cond, scope)
			if f != nil {
				return f
			}
			if !toBool(c) {
				return nil
			}
			bf := vm.execStmt(s.body, scope)
			if bf != nil {
				if bf.kind == flowBreak {
					return nil
				}
				if bf.kind == flowContinue {
					continue
				}
				return bf
			}
		}
	case *doWhileStmt:
		for {
			if f := vm.tick(); f != nil {
				return f
			}
			bf := vm.execStmt(s.body, scope)
			if bf != nil {
				if bf.kind == flowBreak {
					return nil
				}
				if bf.kind != flowContinue {
					return bf
				}
			}
			c, f := vm.evalExpr(s.cond, scope)
			if f != nil {
				return f
			}
			if !toBool(c) {
				return nil
			}
		}
	case *forStmt:
		loopScope := newEnv(scope)
		if s.init != nil {
			if f := vm.execStmt(s.init, loopScope); f != nil {
				return f
			}
		}
		for {
			if f := vm.tick(); f != nil {
				return f
			}
			if s.cond != nil {
				c, f := vm.evalExpr(s.cond, loopScope)
				if f != nil {
					return f
				}
				if !toBool(c) {
					return nil
				}
			}
			bf := vm.execStmt(s.body, loopScope)
			if bf != nil {
				if bf.kind == flowBreak {
					return nil
				}
				if bf.kind != flowContinue {
					return bf
				}
			}
			if s.post != nil {
				if _, f := vm.evalExpr(s.post, loopScope); f != nil {
					return f
				}
			}
		}
	case *forInOfStmt:
		rhsVal, f := vm.evalExpr(s.rhs, scope)
		if f != nil {
			return f
		}
		var items []value
		if s.isOf {
			switch rhsVal.kind {
			case kindArray:
				items = append(items, rhsVal.arr.elems...)
			case kindString:
				for _, r := range rhsVal.s {
					items = append(items, strVal(string(r)))
				}
			case kindMap:
				for _, entry := range rhsVal.mp.entries {
					if entry.active {
						pair, pf := vm.makeArray([]value{entry.key, entry.val})
						if pf != nil {
							return pf
						}
						items = append(items, pair)
					}
				}
			default:
				return vm.throwTypeError("value is not iterable")
			}
		} else {
			switch rhsVal.kind {
			case kindUndefined, kindNull:
				return nil
			case kindObject:
				keys := append([]string(nil), rhsVal.obj.order...)
				for _, k := range keys {
					items = append(items, strVal(k))
				}
			case kindArray:
				for i := range rhsVal.arr.elems {
					items = append(items, strVal(strconv.Itoa(i)))
				}
			}
		}
		for _, item := range items {
			if f := vm.tick(); f != nil {
				return f
			}
			iterScope := newEnv(scope)
			if s.varKind == tokLet || s.varKind == tokConst {
				iterScope.declare(s.name, item, s.varKind == tokConst)
			} else {
				if f := vm.assignIdent(s.name, item, scope); f != nil {
					return f
				}
			}
			bf := vm.execStmt(s.body, iterScope)
			if bf != nil {
				if bf.kind == flowBreak {
					return nil
				}
				if bf.kind != flowContinue {
					return bf
				}
			}
		}
		return nil
	case *breakStmt:
		return &flow{kind: flowBreak}
	case *continueStmt:
		return &flow{kind: flowContinue}
	case *throwStmt:
		v, f := vm.evalExpr(s.value, scope)
		if f != nil {
			return f
		}
		return &flow{kind: flowThrow, val: v}
	case *switchStmt:
		disc, f := vm.evalExpr(s.discriminant, scope)
		if f != nil {
			return f
		}
		swScope := newEnv(scope)
		matchIdx := -1
		defaultIdx := -1
		for i, sc := range s.cases {
			if sc.test == nil {
				defaultIdx = i
				continue
			}
			tv, f := vm.evalExpr(sc.test, swScope)
			if f != nil {
				return f
			}
			if strictEqual(disc, tv) {
				matchIdx = i
				break
			}
		}
		if matchIdx < 0 {
			matchIdx = defaultIdx
		}
		if matchIdx < 0 {
			return nil
		}
		for i := matchIdx; i < len(s.cases); i++ {
			for _, sub := range s.cases[i].stmts {
				bf := vm.execStmt(sub, swScope)
				if bf != nil {
					if bf.kind == flowBreak {
						return nil
					}
					return bf
				}
			}
		}
		return nil
	case *tryStmt:
		tryScope := newEnv(scope)
		var res *flow
		for _, sub := range s.block {
			if f := vm.execStmt(sub, tryScope); f != nil {
				res = f
				break
			}
		}
		if res != nil && res.kind == flowThrow && s.hasCatch {
			catchScope := newEnv(scope)
			if s.catchName != "" {
				catchScope.declare(s.catchName, res.val, false)
			}
			res = nil
			for _, sub := range s.catchBody {
				if f := vm.execStmt(sub, catchScope); f != nil {
					res = f
					break
				}
			}
		}
		if s.hasFinally && (res == nil || res.kind != flowAbort) {
			finScope := newEnv(scope)
			for _, sub := range s.finallyBody {
				if ff := vm.execStmt(sub, finScope); ff != nil {
					return ff
				}
			}
		}
		return res
	default:
		return &flow{kind: flowAbort, err: ErrUnsupported}
	}
}

func (vm *VM) assignIdent(name string, val value, scope *env) *flow {
	if b, ok := scope.lookup(name); ok {
		if b.isCon {
			return vm.throwTypeError("assignment to constant variable")
		}
		b.val = val
		if b == vm.global.vars[name] {
			vm.globalObj.set(name, val)
		}
		return nil
	}
	if len(vm.globalObj.order) >= maxObjectProps {
		return vm.throwError("RangeError", "maximum object property count exceeded")
	}
	vm.defGlobal(name, val)
	return nil
}

// Expression evaluation

func (vm *VM) evalExpr(ex expr, scope *env) (value, *flow) {
	if f := vm.tick(); f != nil {
		return valUndefined, f
	}
	switch e := ex.(type) {
	case *literalExpr:
		switch e.kind {
		case tokNumber:
			return numVal(e.num), nil
		case tokBigInt:
			z, ok := parseBigIntString(e.str)
			if !ok {
				return valUndefined, vm.throwError("SyntaxError", "invalid BigInt literal")
			}
			return vm.makeBigInt(z)
		case tokString:
			if len(e.str) > maxStringBytes {
				return valUndefined, vm.throwError("RangeError", "maximum string length exceeded")
			}
			return strVal(e.str), nil
		case tokTrue:
			return valTrue, nil
		case tokFalse:
			return valFalse, nil
		case tokNull:
			return valNull, nil
		case tokRegex:
			rx, f := compileJSRegex(vm, e.regex.pattern, e.regex.flags)
			if f != nil {
				return valUndefined, f
			}
			return value{kind: kindRegex, rx: rx}, nil
		}
	case *identExpr:
		if b, ok := scope.lookup(e.name); ok {
			return b.val, nil
		}
		return valUndefined, vm.throwError("ReferenceError", e.name+" is not defined")
	case *thisExpr:
		if b, ok := scope.lookup("this"); ok {
			return b.val, nil
		}
		return objVal(vm.globalObj), nil
	case *arrayExpr:
		if len(e.elems) > maxArrayLen {
			return valUndefined, vm.throwError("RangeError", "maximum array length exceeded")
		}
		elems := make([]value, 0, len(e.elems))
		for _, item := range e.elems {
			v, f := vm.evalExpr(item, scope)
			if f != nil {
				return valUndefined, f
			}
			elems = append(elems, v)
		}
		return vm.makeArray(elems)
	case *objectExpr:
		o, of := vm.newCheckedObject()
		if of != nil {
			return valUndefined, of
		}
		for _, prop := range e.props {
			k := prop.key
			if prop.computed != nil {
				kv, f := vm.evalExpr(prop.computed, scope)
				if f != nil {
					return valUndefined, f
				}
				k = toString(kv)
			}
			v, f := vm.evalExpr(prop.value, scope)
			if f != nil {
				return valUndefined, f
			}
			if sf := vm.setObjProp(o, k, v); sf != nil {
				return valUndefined, sf
			}
		}
		return objVal(o), nil
	case *templateExpr:
		var b strings.Builder
		if len(e.quasis[0]) > maxStringBytes {
			return valUndefined, vm.throwError("RangeError", "maximum string length exceeded")
		}
		b.WriteString(e.quasis[0])
		for i, sub := range e.exprs {
			v, f := vm.evalExpr(sub, scope)
			if f != nil {
				return valUndefined, f
			}
			part := toString(v)
			nextQ := e.quasis[i+1]
			if b.Len()+len(part)+len(nextQ) > maxStringBytes {
				return valUndefined, vm.throwError("RangeError", "maximum string length exceeded")
			}
			b.WriteString(part)
			b.WriteString(nextQ)
		}
		return vm.makeString(b.String())
	case *funcExpr:
		if f := vm.chargeAlloc(1); f != nil {
			return valUndefined, f
		}
		return fnVal(&jsFunc{
			name:        e.name,
			params:      e.params,
			body:        e.body,
			hoistedVars: e.hoistedVars,
			hoistedFns:  e.hoistedFns,
			closure:     scope,
			props:       newObject(),
		}), nil
	case *memberExpr:
		obj, f := vm.evalExpr(e.object, scope)
		if f != nil {
			return valUndefined, f
		}
		if e.optional && (obj.kind == kindUndefined || obj.kind == kindNull) {
			return valUndefined, nil
		}
		var key value
		if e.index != nil {
			key, f = vm.evalExpr(e.index, scope)
			if f != nil {
				return valUndefined, f
			}
		} else {
			key = strVal(e.prop)
		}
		return vm.getProperty(obj, key)
	case *callExpr:
		var thisVal = valUndefined
		var fnValOut value
		var f *flow
		if mem, isMem := e.callee.(*memberExpr); isMem && !e.isNew {
			obj, mf := vm.evalExpr(mem.object, scope)
			if mf != nil {
				return valUndefined, mf
			}
			if (mem.optional || e.optional) && (obj.kind == kindUndefined || obj.kind == kindNull) {
				return valUndefined, nil
			}
			var key value
			if mem.index != nil {
				key, mf = vm.evalExpr(mem.index, scope)
				if mf != nil {
					return valUndefined, mf
				}
			} else {
				key = strVal(mem.prop)
			}
			thisVal = obj
			fnValOut, f = vm.getProperty(obj, key)
		} else {
			fnValOut, f = vm.evalExpr(e.callee, scope)
		}
		if f != nil {
			return valUndefined, f
		}
		if e.optional && (fnValOut.kind == kindUndefined || fnValOut.kind == kindNull) {
			return valUndefined, nil
		}
		if fnValOut.kind != kindFunction {
			return valUndefined, vm.throwTypeError("value is not a function")
		}
		args := make([]value, 0, len(e.args))
		for _, a := range e.args {
			av, af := vm.evalExpr(a, scope)
			if af != nil {
				return valUndefined, af
			}
			args = append(args, av)
		}
		if e.isNew {
			inst, inf := vm.newCheckedObject()
			if inf != nil {
				return valUndefined, inf
			}
			prevNew := vm.inNew
			vm.inNew = true
			res, cf := vm.callFunction(fnValOut.fn, objVal(inst), args)
			vm.inNew = prevNew
			if cf != nil {
				return valUndefined, cf
			}
			if res.kind == kindObject || res.kind == kindArray || res.kind == kindFunction || res.kind == kindMap {
				return res, nil
			}
			return objVal(inst), nil
		}
		prevNew := vm.inNew
		vm.inNew = false
		res, cf := vm.callFunction(fnValOut.fn, thisVal, args)
		vm.inNew = prevNew
		return res, cf
	case *unaryExpr:
		if e.op == tokTypeof {
			if id, ok := e.arg.(*identExpr); ok {
				if b, found := scope.lookup(id.name); found {
					return strVal(typeofValue(b.val)), nil
				}
				return strVal("undefined"), nil
			}
			v, f := vm.evalExpr(e.arg, scope)
			if f != nil {
				return valUndefined, f
			}
			return strVal(typeofValue(v)), nil
		}
		if e.op == tokDelete {
			if mem, ok := e.arg.(*memberExpr); ok {
				obj, f := vm.evalExpr(mem.object, scope)
				if f != nil {
					return valUndefined, f
				}
				var k string
				if mem.index != nil {
					kv, kf := vm.evalExpr(mem.index, scope)
					if kf != nil {
						return valUndefined, kf
					}
					k = toString(kv)
				} else {
					k = mem.prop
				}
				if obj.kind == kindObject {
					obj.obj.del(k)
					if obj.obj == vm.globalObj {
						delete(vm.global.vars, k)
					}
				}
				return valTrue, nil
			}
			return valTrue, nil
		}
		v, f := vm.evalExpr(e.arg, scope)
		if f != nil {
			return valUndefined, f
		}
		switch e.op {
		case tokBang:
			return boolVal(!toBool(v)), nil
		case tokPlus:
			if v.kind == kindBigInt {
				return valUndefined, vm.throwTypeError("cannot convert BigInt value to a number")
			}
			return numVal(toNumber(v)), nil
		case tokMinus:
			if v.kind == kindBigInt {
				return vm.makeBigInt(new(big.Int).Neg(v.bi))
			}
			return numVal(-toNumber(v)), nil
		case tokTilde:
			if v.kind == kindBigInt {
				return vm.makeBigInt(new(big.Int).Not(v.bi))
			}
			return numVal(float64(^toInt32(v))), nil
		case tokVoid:
			return valUndefined, nil
		}
	case *updateExpr:
		oldVal, setter, f := vm.resolveLValue(e.arg, scope)
		if f != nil {
			return valUndefined, f
		}
		if oldVal.kind == kindBigInt {
			delta := big.NewInt(1)
			if e.op == tokDec {
				delta.SetInt64(-1)
			}
			nextVal, bf := vm.makeBigInt(new(big.Int).Add(oldVal.bi, delta))
			if bf != nil {
				return valUndefined, bf
			}
			if sf := setter(nextVal); sf != nil {
				return valUndefined, sf
			}
			if e.prefix {
				return nextVal, nil
			}
			return oldVal, nil
		}
		oldNum := toNumber(oldVal)
		newNum := oldNum + 1
		if e.op == tokDec {
			newNum = oldNum - 1
		}
		if sf := setter(numVal(newNum)); sf != nil {
			return valUndefined, sf
		}
		if e.prefix {
			return numVal(newNum), nil
		}
		return numVal(oldNum), nil
	case *assignExpr:
		if e.op == tokAssign {
			if id, ok := e.left.(*identExpr); ok {
				rv, f := vm.evalExpr(e.right, scope)
				if f != nil {
					return valUndefined, f
				}
				if sf := vm.assignIdent(id.name, rv, scope); sf != nil {
					return valUndefined, sf
				}
				return rv, nil
			}
			_, setter, f := vm.resolveLValue(e.left, scope)
			if f != nil {
				return valUndefined, f
			}
			rv, rf := vm.evalExpr(e.right, scope)
			if rf != nil {
				return valUndefined, rf
			}
			if sf := setter(rv); sf != nil {
				return valUndefined, sf
			}
			return rv, nil
		}
		oldVal, setter, f := vm.resolveLValue(e.left, scope)
		if f != nil {
			return valUndefined, f
		}
		rv, rf := vm.evalExpr(e.right, scope)
		if rf != nil {
			return valUndefined, rf
		}
		var next value
		var opf *flow
		switch e.op {
		case tokPlusEq:
			next, opf = vm.evalBinaryOp(tokPlus, oldVal, rv)
		case tokMinusEq:
			next, opf = vm.evalBinaryOp(tokMinus, oldVal, rv)
		case tokStarEq:
			next, opf = vm.evalBinaryOp(tokStar, oldVal, rv)
		case tokSlashEq:
			next, opf = vm.evalBinaryOp(tokSlash, oldVal, rv)
		case tokPercentEq:
			next, opf = vm.evalBinaryOp(tokPercent, oldVal, rv)
		}
		if opf != nil {
			return valUndefined, opf
		}
		if sf := setter(next); sf != nil {
			return valUndefined, sf
		}
		return next, nil
	case *binaryExpr:
		switch e.op {
		case tokAndAnd:
			lv, f := vm.evalExpr(e.left, scope)
			if f != nil {
				return valUndefined, f
			}
			if !toBool(lv) {
				return lv, nil
			}
			return vm.evalExpr(e.right, scope)
		case tokOrOr:
			lv, f := vm.evalExpr(e.left, scope)
			if f != nil {
				return valUndefined, f
			}
			if toBool(lv) {
				return lv, nil
			}
			return vm.evalExpr(e.right, scope)
		case tokNullish:
			lv, f := vm.evalExpr(e.left, scope)
			if f != nil {
				return valUndefined, f
			}
			if lv.kind != kindUndefined && lv.kind != kindNull {
				return lv, nil
			}
			return vm.evalExpr(e.right, scope)
		}
		lv, f := vm.evalExpr(e.left, scope)
		if f != nil {
			return valUndefined, f
		}
		rv, f := vm.evalExpr(e.right, scope)
		if f != nil {
			return valUndefined, f
		}
		return vm.evalBinaryOp(e.op, lv, rv)
	case *condExpr:
		cv, f := vm.evalExpr(e.cond, scope)
		if f != nil {
			return valUndefined, f
		}
		if toBool(cv) {
			return vm.evalExpr(e.cons, scope)
		}
		return vm.evalExpr(e.alt, scope)
	case *seqExpr:
		var last = valUndefined
		for _, sub := range e.exprs {
			var f *flow
			last, f = vm.evalExpr(sub, scope)
			if f != nil {
				return valUndefined, f
			}
		}
		return last, nil
	}
	return valUndefined, &flow{kind: flowAbort, err: ErrUnsupported}
}

func (vm *VM) resolveLValue(lhs expr, scope *env) (value, func(value) *flow, *flow) {
	switch target := lhs.(type) {
	case *identExpr:
		b, ok := scope.lookup(target.name)
		cur := valUndefined
		if ok {
			cur = b.val
		}
		setter := func(v value) *flow {
			return vm.assignIdent(target.name, v, scope)
		}
		return cur, setter, nil
	case *memberExpr:
		obj, f := vm.evalExpr(target.object, scope)
		if f != nil {
			return valUndefined, nil, f
		}
		if obj.kind == kindUndefined || obj.kind == kindNull {
			return valUndefined, nil, vm.throwTypeError("cannot access property of null or undefined")
		}
		var key value
		if target.index != nil {
			key, f = vm.evalExpr(target.index, scope)
			if f != nil {
				return valUndefined, nil, f
			}
		} else {
			key = strVal(target.prop)
		}
		cur, gf := vm.getProperty(obj, key)
		if gf != nil {
			return valUndefined, nil, gf
		}
		setter := func(v value) *flow {
			return vm.setProperty(obj, key, v)
		}
		return cur, setter, nil
	default:
		return valUndefined, nil, vm.throwError("ReferenceError", "invalid assignment left-hand side")
	}
}

func (vm *VM) getProperty(obj value, key value) (value, *flow) {
	if obj.kind == kindUndefined || obj.kind == kindNull {
		return valUndefined, vm.throwTypeError("cannot read property of null or undefined")
	}
	prop := toString(key)

	switch obj.kind {
	case kindObject:
		if v, ok := obj.obj.get(prop); ok {
			return v, nil
		}
		return vm.objectProtoMethod(prop), nil
	case kindArray:
		if prop == "length" {
			return numVal(float64(len(obj.arr.elems))), nil
		}
		if idx, ok := parseArrayIndex(prop); ok {
			if idx < len(obj.arr.elems) {
				return obj.arr.elems[idx], nil
			}
			return valUndefined, nil
		}
		return vm.arrayProtoMethod(prop), nil
	case kindMap:
		if prop == "size" {
			return numVal(float64(obj.mp.size)), nil
		}
		return vm.mapProtoMethod(prop), nil
	case kindString:
		if prop == "length" {
			return numVal(float64(utf16Len(obj.s))), nil
		}
		if idx, ok := parseArrayIndex(prop); ok {
			runes := []rune(obj.s)
			if idx < len(runes) {
				return strVal(string(runes[idx])), nil
			}
			return valUndefined, nil
		}
		return vm.stringProtoMethod(prop), nil
	case kindNumber:
		return vm.numberProtoMethod(prop), nil
	case kindBigInt:
		return vm.bigIntProtoMethod(prop), nil
	case kindFunction:
		if obj.fn.props != nil {
			if v, ok := obj.fn.props.get(prop); ok {
				return v, nil
			}
		}
		return vm.funcProtoMethod(prop), nil
	case kindRegex:
		return vm.regexProtoMethod(prop), nil
	default:
		return valUndefined, nil
	}
}

func (vm *VM) setProperty(obj value, key value, val value) *flow {
	if obj.kind == kindUndefined || obj.kind == kindNull {
		return vm.throwTypeError("cannot set property of null or undefined")
	}
	prop := toString(key)
	switch obj.kind {
	case kindObject:
		return vm.setObjProp(obj.obj, prop, val)
	case kindArray:
		if prop == "length" {
			fn := toNumber(val)
			n := int(fn)
			if float64(n) != fn || n < 0 || n > maxArrayLen {
				return vm.throwError("RangeError", "invalid array length")
			}
			if n < len(obj.arr.elems) {
				obj.arr.elems = obj.arr.elems[:n]
			} else {
				if f := vm.chargeAlloc(1 + (n-len(obj.arr.elems))/64); f != nil {
					return f
				}
				for len(obj.arr.elems) < n {
					obj.arr.elems = append(obj.arr.elems, valUndefined)
				}
			}
			return nil
		}
		if idx, ok := parseArrayIndex(prop); ok {
			if idx >= maxArrayLen {
				return vm.throwError("RangeError", "maximum array length exceeded")
			}
			if idx >= len(obj.arr.elems) {
				if f := vm.chargeAlloc(1 + (idx+1-len(obj.arr.elems))/64); f != nil {
					return f
				}
				for len(obj.arr.elems) <= idx {
					obj.arr.elems = append(obj.arr.elems, valUndefined)
				}
			}
			obj.arr.elems[idx] = val
		}
	case kindFunction:
		if obj.fn.props == nil {
			obj.fn.props = newObject()
		}
		return vm.setObjProp(obj.fn.props, prop, val)
	}
	return nil
}

func parseArrayIndex(s string) (int, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 1<<20 {
		return 0, false
	}
	return n, true
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Operators & Conversions

func parseBigIntString(raw string) (*big.Int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return big.NewInt(0), true
	}
	base := 10
	neg := false
	digits := s
	if len(digits) >= 2 && digits[0] == '0' {
		switch digits[1] {
		case 'x', 'X':
			base = 16
			digits = digits[2:]
		case 'o', 'O':
			base = 8
			digits = digits[2:]
		case 'b', 'B':
			base = 2
			digits = digits[2:]
		}
	}
	if base == 10 && len(digits) > 0 && (digits[0] == '+' || digits[0] == '-') {
		neg = digits[0] == '-'
		digits = digits[1:]
	}
	if digits == "" {
		return nil, false
	}
	firstNonZero := -1
	for i := 0; i < len(digits); i++ {
		d := digitVal(digits[i])
		if d < 0 || d >= base {
			return nil, false
		}
		if d != 0 && firstNonZero < 0 {
			firstNonZero = i
		}
	}
	if firstNonZero < 0 {
		return big.NewInt(0), true
	}
	sig := digits[firstNonZero:]
	maxSig := 5000
	if base == 2 {
		maxSig = maxBigIntBits + 2
	} else if base == 8 {
		maxSig = (maxBigIntBits / 3) + 4
	}
	if len(sig) > maxSig {
		return new(big.Int).Lsh(big.NewInt(1), maxBigIntBits+1), true
	}
	z, ok := new(big.Int).SetString(sig, base)
	if !ok {
		return nil, false
	}
	if neg {
		z.Neg(z)
	}
	return z, true
}

func cmpBigIntNumber(bi *big.Int, n float64) (int, bool) {
	if math.IsNaN(n) {
		return 0, false
	}
	if math.IsInf(n, 1) {
		return -1, true
	}
	if math.IsInf(n, -1) {
		return 1, true
	}
	if bi.BitLen() > 1025 {
		if bi.Sign() >= 0 {
			return 1, true
		}
		return -1, true
	}
	rb := new(big.Rat).SetInt(bi)
	rn := new(big.Rat).SetFloat64(n)
	if rn == nil {
		return 0, false
	}
	return rb.Cmp(rn), true
}

func typeofValue(v value) string {
	switch v.kind {
	case kindUndefined:
		return "undefined"
	case kindBool:
		return "boolean"
	case kindNumber:
		return "number"
	case kindBigInt:
		return "bigint"
	case kindString:
		return "string"
	case kindFunction:
		return "function"
	default:
		return "object"
	}
}

func toBool(v value) bool {
	switch v.kind {
	case kindUndefined, kindNull:
		return false
	case kindBool:
		return v.b
	case kindNumber:
		return v.n != 0 && !math.IsNaN(v.n)
	case kindBigInt:
		return v.bi.Sign() != 0
	case kindString:
		return v.s != ""
	default:
		return true
	}
}

func toNumber(v value) float64 {
	switch v.kind {
	case kindUndefined:
		return math.NaN()
	case kindNull:
		return 0
	case kindBool:
		if v.b {
			return 1
		}
		return 0
	case kindNumber:
		return v.n
	case kindString:
		s := strings.TrimSpace(v.s)
		if s == "" {
			return 0
		}
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			u, err := strconv.ParseUint(s[2:], 16, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(u)
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	case kindArray:
		if len(v.arr.elems) == 0 {
			return 0
		}
		if len(v.arr.elems) == 1 {
			return toNumber(v.arr.elems[0])
		}
		return math.NaN()
	default:
		return math.NaN()
	}
}

func toInt32(v value) int32 {
	f := toNumber(v)
	if math.IsNaN(f) || math.IsInf(f, 0) || f == 0 {
		return 0
	}
	n := math.Trunc(f)
	m := math.Mod(n, 4294967296.0)
	if m < 0 {
		m += 4294967296.0
	}
	if m >= 2147483648.0 {
		return int32(m - 4294967296.0)
	}
	return int32(m)
}

func toUint32(v value) uint32 {
	return uint32(toInt32(v))
}

func toString(v value) string {
	return toStringSeen(v, nil)
}

func toStringSeen(v value, seen map[*jsArray]bool) string {
	switch v.kind {
	case kindUndefined:
		return "undefined"
	case kindNull:
		return "null"
	case kindBool:
		if v.b {
			return "true"
		}
		return "false"
	case kindNumber:
		return formatJSNumber(v.n)
	case kindBigInt:
		return v.bi.String()
	case kindString:
		return v.s
	case kindArray:
		s, _ := joinArrayBounded(v.arr, ",", seen, maxStringBytes+1)
		return s
	case kindMap:
		return "[object Map]"
	default:
		return "[object Object]"
	}
}

func joinArrayBounded(a *jsArray, sep string, seen map[*jsArray]bool, limit int) (string, bool) {
	if a == nil || len(a.elems) == 0 {
		return "", true
	}
	if seen == nil {
		seen = make(map[*jsArray]bool)
	}
	if seen[a] {
		return "", true
	}
	seen[a] = true
	defer delete(seen, a)

	var b strings.Builder
	for i, el := range a.elems {
		if i > 0 {
			if b.Len()+len(sep) > limit {
				return "", false
			}
			b.WriteString(sep)
		}
		if el.kind == kindUndefined || el.kind == kindNull {
			continue
		}
		var part string
		if el.kind == kindArray {
			sub, ok := joinArrayBounded(el.arr, ",", seen, limit-b.Len())
			if !ok {
				return "", false
			}
			part = sub
		} else {
			part = toStringSeen(el, seen)
		}
		if b.Len()+len(part) > limit {
			return "", false
		}
		b.WriteString(part)
	}
	return b.String(), true
}

func formatJSNumber(n float64) string {
	if math.IsNaN(n) {
		return "NaN"
	}
	if math.IsInf(n, 1) {
		return "Infinity"
	}
	if math.IsInf(n, -1) {
		return "-Infinity"
	}
	if n == 0 {
		return "0"
	}
	abs := math.Abs(n)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	s := strconv.FormatFloat(n, 'e', -1, 64)
	if i := strings.IndexByte(s, 'e'); i >= 0 && i+3 < len(s) && s[i+2] == '0' {
		s = s[:i+2] + s[i+3:]
	}
	return s
}

func toPrimitive(v value) value {
	switch v.kind {
	case kindObject, kindArray, kindMap, kindFunction, kindRegex:
		return strVal(toString(v))
	default:
		return v
	}
}

func (vm *VM) evalAdd(a, b value) (value, *flow) {
	pa := toPrimitive(a)
	pb := toPrimitive(b)
	if pa.kind == kindString || pb.kind == kindString {
		if pa.kind == kindArray {
			if _, ok := joinArrayBounded(pa.arr, ",", nil, maxStringBytes); !ok {
				return valUndefined, vm.throwError("RangeError", "maximum string length exceeded")
			}
		}
		if pb.kind == kindArray {
			if _, ok := joinArrayBounded(pb.arr, ",", nil, maxStringBytes); !ok {
				return valUndefined, vm.throwError("RangeError", "maximum string length exceeded")
			}
		}
		sa := toString(pa)
		sb := toString(pb)
		if len(sa) > maxStringBytes || len(sb) > maxStringBytes || len(sa)+len(sb) > maxStringBytes {
			return valUndefined, vm.throwError("RangeError", "maximum string length exceeded")
		}
		return vm.makeString(sa + sb)
	}
	if pa.kind == kindBigInt && pb.kind == kindBigInt {
		return vm.makeBigInt(new(big.Int).Add(pa.bi, pb.bi))
	}
	if pa.kind == kindBigInt || pb.kind == kindBigInt {
		return valUndefined, vm.throwTypeError("cannot mix BigInt and other types, use explicit conversions")
	}
	return numVal(toNumber(pa) + toNumber(pb)), nil
}

func strictEqual(a, b value) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case kindUndefined, kindNull:
		return true
	case kindBool:
		return a.b == b.b
	case kindNumber:
		return a.n == b.n
	case kindBigInt:
		return a.bi.Cmp(b.bi) == 0
	case kindString:
		return a.s == b.s
	case kindObject:
		return a.obj == b.obj
	case kindArray:
		return a.arr == b.arr
	case kindMap:
		return a.mp == b.mp
	case kindFunction:
		return a.fn == b.fn
	case kindRegex:
		return a.rx == b.rx
	default:
		return false
	}
}

func abstractEqual(a, b value) bool {
	if a.kind == b.kind {
		return strictEqual(a, b)
	}
	if (a.kind == kindUndefined && b.kind == kindNull) || (a.kind == kindNull && b.kind == kindUndefined) {
		return true
	}
	if a.kind == kindBigInt && b.kind == kindString {
		z, ok := parseBigIntString(b.s)
		return ok && z.BitLen() <= maxBigIntBits && a.bi.Cmp(z) == 0
	}
	if a.kind == kindString && b.kind == kindBigInt {
		z, ok := parseBigIntString(a.s)
		return ok && z.BitLen() <= maxBigIntBits && z.Cmp(b.bi) == 0
	}
	if a.kind == kindBigInt && b.kind == kindNumber {
		cmp, ok := cmpBigIntNumber(a.bi, b.n)
		return ok && cmp == 0
	}
	if a.kind == kindNumber && b.kind == kindBigInt {
		cmp, ok := cmpBigIntNumber(b.bi, a.n)
		return ok && cmp == 0
	}
	if a.kind == kindNumber && b.kind == kindString {
		return a.n == toNumber(b)
	}
	if a.kind == kindString && b.kind == kindNumber {
		return toNumber(a) == b.n
	}
	if a.kind == kindBool {
		return abstractEqual(numVal(toNumber(a)), b)
	}
	if b.kind == kindBool {
		return abstractEqual(a, numVal(toNumber(b)))
	}
	if (a.kind == kindString || a.kind == kindNumber || a.kind == kindBigInt) && (b.kind == kindObject || b.kind == kindArray || b.kind == kindMap) {
		return abstractEqual(a, toPrimitive(b))
	}
	if (a.kind == kindObject || a.kind == kindArray || a.kind == kindMap) && (b.kind == kindString || b.kind == kindNumber || b.kind == kindBigInt) {
		return abstractEqual(toPrimitive(a), b)
	}
	return false
}

func (vm *VM) evalBigIntOp(op tokenKind, a, b *big.Int) (value, *flow) {
	switch op {
	case tokMinus:
		return vm.makeBigInt(new(big.Int).Sub(a, b))
	case tokStar:
		if a.Sign() != 0 && b.Sign() != 0 && a.BitLen()+b.BitLen()-1 > maxBigIntBits {
			return valUndefined, vm.throwError("RangeError", "maximum BigInt size exceeded")
		}
		return vm.makeBigInt(new(big.Int).Mul(a, b))
	case tokSlash:
		if b.Sign() == 0 {
			return valUndefined, vm.throwError("RangeError", "division by zero")
		}
		return vm.makeBigInt(new(big.Int).Quo(a, b))
	case tokPercent:
		if b.Sign() == 0 {
			return valUndefined, vm.throwError("RangeError", "division by zero")
		}
		return vm.makeBigInt(new(big.Int).Rem(a, b))
	case tokStarStar:
		if b.Sign() < 0 {
			return valUndefined, vm.throwError("RangeError", "exponent must be positive")
		}
		if b.Sign() == 0 {
			return vm.makeBigInt(big.NewInt(1))
		}
		if a.Sign() == 0 {
			return vm.makeBigInt(big.NewInt(0))
		}
		if a.Cmp(big.NewInt(1)) == 0 {
			return vm.makeBigInt(big.NewInt(1))
		}
		if a.Cmp(big.NewInt(-1)) == 0 {
			if b.Bit(0) == 1 {
				return vm.makeBigInt(big.NewInt(-1))
			}
			return vm.makeBigInt(big.NewInt(1))
		}
		if !b.IsInt64() || b.Int64() > maxBigIntBits {
			return valUndefined, vm.throwError("RangeError", "maximum BigInt size exceeded")
		}
		exp := b.Int64()
		if int64(a.BitLen()-1)*exp > maxBigIntBits {
			return valUndefined, vm.throwError("RangeError", "maximum BigInt size exceeded")
		}
		return vm.makeBigInt(new(big.Int).Exp(a, b, nil))
	case tokPipe:
		return vm.makeBigInt(new(big.Int).Or(a, b))
	case tokCaret:
		return vm.makeBigInt(new(big.Int).Xor(a, b))
	case tokAmp:
		return vm.makeBigInt(new(big.Int).And(a, b))
	case tokLShift, tokRShift:
		shift := new(big.Int).Set(b)
		if op == tokRShift {
			shift.Neg(shift)
		}
		if shift.Sign() > 0 {
			if a.Sign() == 0 {
				return vm.makeBigInt(big.NewInt(0))
			}
			if !shift.IsInt64() || shift.Int64()+int64(a.BitLen()) > maxBigIntBits+1 {
				return valUndefined, vm.throwError("RangeError", "maximum BigInt size exceeded")
			}
			return vm.makeBigInt(new(big.Int).Lsh(a, uint(shift.Int64())))
		}
		if shift.Sign() < 0 {
			rsh := new(big.Int).Neg(shift)
			if !rsh.IsInt64() || rsh.Int64() > int64(a.BitLen())+1 {
				if a.Sign() < 0 {
					return vm.makeBigInt(big.NewInt(-1))
				}
				return vm.makeBigInt(big.NewInt(0))
			}
			return vm.makeBigInt(new(big.Int).Rsh(a, uint(rsh.Int64())))
		}
		return vm.makeBigInt(new(big.Int).Set(a))
	case tokURShift:
		return valUndefined, vm.throwTypeError("BigInts have no unsigned right shift, use >> instead")
	}
	return valUndefined, &flow{kind: flowAbort, err: ErrUnsupported}
}

func cmpResult(op tokenKind, cmp int) value {
	switch op {
	case tokLt:
		return boolVal(cmp < 0)
	case tokLtEq:
		return boolVal(cmp <= 0)
	case tokGt:
		return boolVal(cmp > 0)
	case tokGtEq:
		return boolVal(cmp >= 0)
	}
	return valFalse
}

func (vm *VM) evalBinaryOp(op tokenKind, a, b value) (value, *flow) {
	switch op {
	case tokPlus:
		return vm.evalAdd(a, b)
	case tokMinus, tokStar, tokSlash, tokPercent, tokStarStar, tokPipe, tokCaret, tokAmp, tokLShift, tokRShift, tokURShift:
		pa := toPrimitive(a)
		pb := toPrimitive(b)
		if pa.kind == kindBigInt || pb.kind == kindBigInt {
			if pa.kind != kindBigInt || pb.kind != kindBigInt {
				return valUndefined, vm.throwTypeError("cannot mix BigInt and other types, use explicit conversions")
			}
			return vm.evalBigIntOp(op, pa.bi, pb.bi)
		}
		switch op {
		case tokMinus:
			return numVal(toNumber(pa) - toNumber(pb)), nil
		case tokStar:
			return numVal(toNumber(pa) * toNumber(pb)), nil
		case tokSlash:
			return numVal(toNumber(pa) / toNumber(pb)), nil
		case tokPercent:
			return numVal(math.Mod(toNumber(pa), toNumber(pb))), nil
		case tokStarStar:
			return numVal(math.Pow(toNumber(pa), toNumber(pb))), nil
		case tokPipe:
			return numVal(float64(toInt32(pa) | toInt32(pb))), nil
		case tokCaret:
			return numVal(float64(toInt32(pa) ^ toInt32(pb))), nil
		case tokAmp:
			return numVal(float64(toInt32(pa) & toInt32(pb))), nil
		case tokLShift:
			return numVal(float64(toInt32(pa) << (toUint32(pb) & 0x1f))), nil
		case tokRShift:
			return numVal(float64(toInt32(pa) >> (toUint32(pb) & 0x1f))), nil
		case tokURShift:
			return numVal(float64(toUint32(pa) >> (toUint32(pb) & 0x1f))), nil
		}
	case tokEqEq:
		return boolVal(abstractEqual(a, b)), nil
	case tokNotEq:
		return boolVal(!abstractEqual(a, b)), nil
	case tokEqEqEq:
		return boolVal(strictEqual(a, b)), nil
	case tokNotEqEq:
		return boolVal(!strictEqual(a, b)), nil
	case tokLt, tokLtEq, tokGt, tokGtEq:
		pa := toPrimitive(a)
		pb := toPrimitive(b)
		if pa.kind == kindString && pb.kind == kindString {
			switch op {
			case tokLt:
				return boolVal(pa.s < pb.s), nil
			case tokLtEq:
				return boolVal(pa.s <= pb.s), nil
			case tokGt:
				return boolVal(pa.s > pb.s), nil
			case tokGtEq:
				return boolVal(pa.s >= pb.s), nil
			}
		}
		if pa.kind == kindBigInt && pb.kind == kindBigInt {
			return cmpResult(op, pa.bi.Cmp(pb.bi)), nil
		}
		if pa.kind == kindBigInt && pb.kind == kindString {
			z, ok := parseBigIntString(pb.s)
			if !ok {
				return valFalse, nil
			}
			return cmpResult(op, pa.bi.Cmp(z)), nil
		}
		if pa.kind == kindString && pb.kind == kindBigInt {
			z, ok := parseBigIntString(pa.s)
			if !ok {
				return valFalse, nil
			}
			return cmpResult(op, z.Cmp(pb.bi)), nil
		}
		if pa.kind == kindBigInt {
			cmp, ok := cmpBigIntNumber(pa.bi, toNumber(pb))
			if !ok {
				return valFalse, nil
			}
			return cmpResult(op, cmp), nil
		}
		if pb.kind == kindBigInt {
			cmp, ok := cmpBigIntNumber(pb.bi, toNumber(pa))
			if !ok {
				return valFalse, nil
			}
			return cmpResult(op, -cmp), nil
		}
		na := toNumber(pa)
		nb := toNumber(pb)
		if math.IsNaN(na) || math.IsNaN(nb) {
			return valFalse, nil
		}
		switch op {
		case tokLt:
			return boolVal(na < nb), nil
		case tokLtEq:
			return boolVal(na <= nb), nil
		case tokGt:
			return boolVal(na > nb), nil
		case tokGtEq:
			return boolVal(na >= nb), nil
		}
	case tokIn:
		k := toString(a)
		switch b.kind {
		case kindObject:
			_, ok := b.obj.get(k)
			return boolVal(ok), nil
		case kindArray:
			if k == "length" {
				return valTrue, nil
			}
			idx, ok := parseArrayIndex(k)
			return boolVal(ok && idx < len(b.arr.elems)), nil
		default:
			return valUndefined, vm.throwTypeError("cannot use 'in' operator on primitive")
		}
	case tokInstanceof:
		if b.kind != kindFunction {
			return valUndefined, vm.throwTypeError("right-hand side of instanceof is not callable")
		}
		if b.fn.name == "Array" {
			return boolVal(a.kind == kindArray), nil
		}
		if b.fn.name == "Map" {
			return boolVal(a.kind == kindMap), nil
		}
		return boolVal(a.kind == kindObject || a.kind == kindArray || a.kind == kindMap || a.kind == kindFunction), nil
	}
	return valUndefined, &flow{kind: flowAbort, err: ErrUnsupported}
}

const tokBitOr = tokPipe

// Built-ins and Prototype Methods

func (vm *VM) installBuiltins() {
	vm.defGlobal("undefined", valUndefined)
	vm.defGlobal("NaN", valNaN)
	vm.defGlobal("Infinity", numVal(math.Inf(1)))
	vm.defGlobal("globalThis", objVal(vm.globalObj))

	vm.defGlobal("isNaN", makeNative("isNaN", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valTrue, nil
		}
		if args[0].kind == kindBigInt {
			return valUndefined, v.throwTypeError("cannot convert BigInt value to a number")
		}
		return boolVal(math.IsNaN(toNumber(args[0]))), nil
	}))
	vm.defGlobal("isFinite", makeNative("isFinite", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valFalse, nil
		}
		if args[0].kind == kindBigInt {
			return valUndefined, v.throwTypeError("cannot convert BigInt value to a number")
		}
		n := toNumber(args[0])
		return boolVal(!math.IsNaN(n) && !math.IsInf(n, 0)), nil
	}))

	parseIntFn := makeNative("parseInt", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valNaN, nil
		}
		s := strings.TrimSpace(toString(args[0]))
		radix := 10
		if len(args) > 1 && args[1].kind != kindUndefined {
			radix = int(toInt32(args[1]))
		}
		if s == "" {
			return valNaN, nil
		}
		sign := 1.0
		if s[0] == '-' {
			sign = -1
			s = s[1:]
		} else if s[0] == '+' {
			s = s[1:]
		}
		if (radix == 0 || radix == 16) && (strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X")) {
			radix = 16
			s = s[2:]
		} else if radix == 0 {
			radix = 10
		}
		if radix < 2 || radix > 36 {
			return valNaN, nil
		}
		end := 0
		for end < len(s) {
			d := digitVal(s[end])
			if d < 0 || d >= radix {
				break
			}
			end++
		}
		if end == 0 {
			return valNaN, nil
		}
		var acc float64
		for i := 0; i < end; i++ {
			acc = acc*float64(radix) + float64(digitVal(s[i]))
		}
		return numVal(sign * acc), nil
	})
	parseFloatFn := makeNative("parseFloat", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valNaN, nil
		}
		s := strings.TrimSpace(toString(args[0]))
		end := 0
		if end < len(s) && (s[end] == '+' || s[end] == '-') {
			end++
		}
		hasDigits := false
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
			hasDigits = true
		}
		if end < len(s) && s[end] == '.' {
			end++
			for end < len(s) && s[end] >= '0' && s[end] <= '9' {
				end++
				hasDigits = true
			}
		}
		if !hasDigits {
			return valNaN, nil
		}
		if end < len(s) && (s[end] == 'e' || s[end] == 'E') {
			expPos := end + 1
			if expPos < len(s) && (s[expPos] == '+' || s[expPos] == '-') {
				expPos++
			}
			expDigits := 0
			for expPos < len(s) && s[expPos] >= '0' && s[expPos] <= '9' {
				expPos++
				expDigits++
			}
			if expDigits > 0 {
				end = expPos
			}
		}
		f, err := strconv.ParseFloat(s[:end], 64)
		if err != nil {
			return valNaN, nil
		}
		return numVal(f), nil
	})
	vm.defGlobal("parseInt", parseIntFn)
	vm.defGlobal("parseFloat", parseFloatFn)

	// Number
	numCtor := makeNative("Number", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return numVal(0), nil
		}
		if args[0].kind == kindBigInt {
			f, _ := new(big.Float).SetInt(args[0].bi).Float64()
			return numVal(f), nil
		}
		return numVal(toNumber(args[0])), nil
	})
	numCtor.fn.props.set("isInteger", makeNative("isInteger", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind != kindNumber {
			return valFalse, nil
		}
		n := args[0].n
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return valFalse, nil
		}
		return boolVal(math.Trunc(n) == n), nil
	}))
	numCtor.fn.props.set("isFinite", makeNative("isFinite", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind != kindNumber {
			return valFalse, nil
		}
		n := args[0].n
		return boolVal(!math.IsNaN(n) && !math.IsInf(n, 0)), nil
	}))
	numCtor.fn.props.set("isNaN", makeNative("isNaN", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind != kindNumber {
			return valFalse, nil
		}
		return boolVal(math.IsNaN(args[0].n)), nil
	}))
	numCtor.fn.props.set("parseInt", parseIntFn)
	numCtor.fn.props.set("parseFloat", parseFloatFn)
	numCtor.fn.props.set("MAX_SAFE_INTEGER", numVal(9007199254740991))
	numCtor.fn.props.set("MIN_SAFE_INTEGER", numVal(-9007199254740991))
	vm.defGlobal("Number", numCtor)

	// BigInt
	vm.defGlobal("BigInt", makeNative("BigInt", func(v *VM, _ value, args []value) (value, *flow) {
		if v.inNew {
			return valUndefined, v.throwTypeError("BigInt is not a constructor")
		}
		if len(args) == 0 {
			return valUndefined, v.throwTypeError("cannot convert undefined to a BigInt")
		}
		arg := toPrimitive(args[0])
		switch arg.kind {
		case kindBigInt:
			return arg, nil
		case kindBool:
			if arg.b {
				return v.makeBigInt(big.NewInt(1))
			}
			return v.makeBigInt(big.NewInt(0))
		case kindNumber:
			if math.IsNaN(arg.n) || math.IsInf(arg.n, 0) || math.Trunc(arg.n) != arg.n {
				return valUndefined, v.throwError("RangeError", "number cannot be converted to a BigInt because it is not an integer")
			}
			r := new(big.Rat).SetFloat64(arg.n)
			if r == nil || !r.IsInt() {
				return valUndefined, v.throwError("RangeError", "number cannot be converted to a BigInt because it is not an integer")
			}
			return v.makeBigInt(new(big.Int).Set(r.Num()))
		case kindString:
			z, ok := parseBigIntString(arg.s)
			if !ok {
				return valUndefined, v.throwError("SyntaxError", "cannot convert string to a BigInt")
			}
			return v.makeBigInt(z)
		default:
			return valUndefined, v.throwTypeError("cannot convert value to a BigInt")
		}
	}))

	// String & Boolean
	vm.defGlobal("String", makeNative("String", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return strVal(""), nil
		}
		if args[0].kind == kindArray {
			joined, ok := joinArrayBounded(args[0].arr, ",", nil, maxStringBytes)
			if !ok {
				return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
			}
			return v.makeString(joined)
		}
		return v.makeString(toString(args[0]))
	}))
	vm.defGlobal("Boolean", makeNative("Boolean", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valFalse, nil
		}
		return boolVal(toBool(args[0])), nil
	}))

	// Math (deterministic; Math.random throws TypeError)
	mathObj := newObject()
	mathObj.set("PI", numVal(math.Pi))
	mathObj.set("E", numVal(math.E))
	mathObj.set("random", makeNative("random", func(v *VM, _ value, _ []value) (value, *flow) {
		return valUndefined, v.throwTypeError("Math.random is not available")
	}))
	mathObj.set("abs", makeNative("abs", unaryMath(math.Abs)))
	mathObj.set("floor", makeNative("floor", unaryMath(math.Floor)))
	mathObj.set("ceil", makeNative("ceil", unaryMath(math.Ceil)))
	mathObj.set("trunc", makeNative("trunc", unaryMath(math.Trunc)))
	mathObj.set("sqrt", makeNative("sqrt", unaryMath(math.Sqrt)))
	mathObj.set("round", makeNative("round", unaryMath(jsRound)))
	mathObj.set("pow", makeNative("pow", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) < 2 {
			return valNaN, nil
		}
		if args[0].kind == kindBigInt || args[1].kind == kindBigInt {
			return valUndefined, v.throwTypeError("cannot convert BigInt value to a number")
		}
		return numVal(math.Pow(toNumber(args[0]), toNumber(args[1]))), nil
	}))
	mathObj.set("min", makeNative("min", func(v *VM, _ value, args []value) (value, *flow) {
		res := math.Inf(1)
		for _, a := range args {
			if a.kind == kindBigInt {
				return valUndefined, v.throwTypeError("cannot convert BigInt value to a number")
			}
			n := toNumber(a)
			if math.IsNaN(n) {
				return valNaN, nil
			}
			if n < res || (n == 0 && res == 0 && math.Signbit(n)) {
				res = n
			}
		}
		return numVal(res), nil
	}))
	mathObj.set("max", makeNative("max", func(v *VM, _ value, args []value) (value, *flow) {
		res := math.Inf(-1)
		for _, a := range args {
			if a.kind == kindBigInt {
				return valUndefined, v.throwTypeError("cannot convert BigInt value to a number")
			}
			n := toNumber(a)
			if math.IsNaN(n) {
				return valNaN, nil
			}
			if n > res || (n == 0 && res == 0 && !math.Signbit(n)) {
				res = n
			}
		}
		return numVal(res), nil
	}))
	vm.defGlobal("Math", objVal(mathObj))

	// Array
	arrCtor := makeNative("Array", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 1 && args[0].kind == kindNumber {
			n := int(args[0].n)
			if float64(n) != args[0].n || n < 0 || n > maxArrayLen {
				return valUndefined, v.throwError("RangeError", "invalid array length")
			}
			elems := make([]value, n)
			for i := range elems {
				elems[i] = valUndefined
			}
			return v.makeArray(elems)
		}
		elems := make([]value, len(args))
		copy(elems, args)
		return v.makeArray(elems)
	})
	arrCtor.fn.props.set("isArray", makeNative("isArray", func(_ *VM, _ value, args []value) (value, *flow) {
		return boolVal(len(args) > 0 && args[0].kind == kindArray), nil
	}))
	arrCtor.fn.props.set("from", makeNative("from", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind == kindUndefined || args[0].kind == kindNull {
			return valUndefined, v.throwTypeError("Array.from requires an array-like or iterable object")
		}
		var mapFn *jsFunc
		thisArg := valUndefined
		if len(args) >= 2 && args[1].kind != kindUndefined {
			if args[1].kind != kindFunction {
				return valUndefined, v.throwTypeError("Array.from: when provided, the second argument must be a function")
			}
			mapFn = args[1].fn
			if len(args) >= 3 {
				thisArg = args[2]
			}
		}
		var raw []value
		src := args[0]
		switch src.kind {
		case kindString:
			if utf8.RuneCountInString(src.s) > maxArrayLen {
				return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
			}
			for _, r := range src.s {
				raw = append(raw, strVal(string(r)))
			}
		case kindArray:
			if len(src.arr.elems) > maxArrayLen {
				return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
			}
			raw = append(raw, src.arr.elems...)
		case kindMap:
			if src.mp.size > maxArrayLen {
				return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
			}
			for _, entry := range src.mp.entries {
				if entry.active {
					pair, pf := v.makeArray([]value{entry.key, entry.val})
					if pf != nil {
						return valUndefined, pf
					}
					raw = append(raw, pair)
				}
			}
		case kindObject:
			if lv, ok := src.obj.get("length"); ok {
				ln := int(toNumber(lv))
				if ln < 0 {
					ln = 0
				}
				if ln > maxArrayLen {
					return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
				}
				for i := 0; i < ln; i++ {
					el, exists := src.obj.get(strconv.Itoa(i))
					if !exists {
						el = valUndefined
					}
					raw = append(raw, el)
				}
			}
		}
		if mapFn != nil {
			mapped := make([]value, len(raw))
			for i, el := range raw {
				mv, f := v.callFunction(mapFn, thisArg, []value{el, numVal(float64(i))})
				if f != nil {
					return valUndefined, f
				}
				mapped[i] = mv
			}
			raw = mapped
		}
		return v.makeArray(raw)
	}))
	vm.defGlobal("Array", arrCtor)

	// Map
	mapCtor := makeNative("Map", func(v *VM, _ value, args []value) (value, *flow) {
		if !v.inNew {
			return valUndefined, v.throwTypeError("Constructor Map requires 'new'")
		}
		if f := v.chargeAlloc(1); f != nil {
			return valUndefined, f
		}
		m := &jsMap{}
		if len(args) > 0 && args[0].kind != kindUndefined && args[0].kind != kindNull {
			switch args[0].kind {
			case kindArray:
				for _, item := range args[0].arr.elems {
					if item.kind != kindArray {
						return valUndefined, v.throwTypeError("Iterator value is not an entry object")
					}
					var k, val = valUndefined, valUndefined
					if len(item.arr.elems) > 0 {
						k = item.arr.elems[0]
					}
					if len(item.arr.elems) > 1 {
						val = item.arr.elems[1]
					}
					if sf := m.set(v, k, val); sf != nil {
						return valUndefined, sf
					}
				}
			case kindMap:
				for _, entry := range args[0].mp.entries {
					if entry.active {
						if sf := m.set(v, entry.key, entry.val); sf != nil {
							return valUndefined, sf
						}
					}
				}
			default:
				return valUndefined, v.throwTypeError("iterable is not iterable")
			}
		}
		return mapVal(m), nil
	})
	vm.defGlobal("Map", mapCtor)

	// Object & Object.prototype
	objCtor := makeNative("Object", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind == kindUndefined || args[0].kind == kindNull {
			o, f := v.newCheckedObject()
			if f != nil {
				return valUndefined, f
			}
			return objVal(o), nil
		}
		return args[0], nil
	})
	objProto := newObject()
	objProto.set("hasOwnProperty", makeNative("hasOwnProperty", hasOwnPropNative))
	objProto.set("toString", makeNative("toString", objToStringNative))
	objCtor.fn.props.set("prototype", objVal(objProto))

	objCtor.fn.props.set("keys", makeNative("keys", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind == kindUndefined || args[0].kind == kindNull {
			return valUndefined, v.throwTypeError("cannot convert undefined or null to object")
		}
		var elems []value
		if args[0].kind == kindObject {
			for _, k := range args[0].obj.order {
				elems = append(elems, strVal(k))
			}
		} else if args[0].kind == kindArray {
			for i := range args[0].arr.elems {
				elems = append(elems, strVal(strconv.Itoa(i)))
			}
		}
		return v.makeArray(elems)
	}))
	objCtor.fn.props.set("values", makeNative("values", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind == kindUndefined || args[0].kind == kindNull {
			return valUndefined, v.throwTypeError("cannot convert undefined or null to object")
		}
		var elems []value
		if args[0].kind == kindObject {
			for _, k := range args[0].obj.order {
				elems = append(elems, args[0].obj.props[k])
			}
		} else if args[0].kind == kindArray {
			elems = append(elems, args[0].arr.elems...)
		}
		return v.makeArray(elems)
	}))
	objCtor.fn.props.set("entries", makeNative("entries", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind == kindUndefined || args[0].kind == kindNull {
			return valUndefined, v.throwTypeError("cannot convert undefined or null to object")
		}
		var elems []value
		if args[0].kind == kindObject {
			for _, k := range args[0].obj.order {
				pair, pf := v.makeArray([]value{strVal(k), args[0].obj.props[k]})
				if pf != nil {
					return valUndefined, pf
				}
				elems = append(elems, pair)
			}
		} else if args[0].kind == kindArray {
			for i, el := range args[0].arr.elems {
				pair, pf := v.makeArray([]value{strVal(strconv.Itoa(i)), el})
				if pf != nil {
					return valUndefined, pf
				}
				elems = append(elems, pair)
			}
		}
		return v.makeArray(elems)
	}))
	objCtor.fn.props.set("assign", makeNative("assign", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind != kindObject {
			return valUndefined, v.throwTypeError("target must be an object")
		}
		dst := args[0].obj
		for _, src := range args[1:] {
			if src.kind == kindObject {
				for _, k := range src.obj.order {
					if sf := v.setObjProp(dst, k, src.obj.props[k]); sf != nil {
						return valUndefined, sf
					}
				}
			}
		}
		return args[0], nil
	}))
	objCtor.fn.props.set("freeze", makeNative("freeze", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valUndefined, nil
		}
		return args[0], nil
	}))
	vm.defGlobal("Object", objCtor)

	// JSON
	jsonObj := newObject()
	jsonObj.set("parse", makeNative("parse", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valUndefined, v.throwError("SyntaxError", "unexpected end of JSON input")
		}
		parsed, err := v.parseJSONValueChecked([]byte(toString(args[0])))
		if err != nil {
			if errors.Is(err, errJSONLimit) {
				return valUndefined, v.throwError("RangeError", err.Error())
			}
			return valUndefined, v.throwError("SyntaxError", "invalid JSON")
		}
		return parsed, nil
	}))
	jsonObj.set("stringify", makeNative("stringify", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valUndefined, nil
		}
		out, ok, f := stringifyValue(v, args[0])
		if f != nil {
			return valUndefined, f
		}
		if !ok {
			return valUndefined, nil
		}
		return v.makeString(out)
	}))
	vm.defGlobal("JSON", objVal(jsonObj))

	// Error constructors
	for _, errName := range []string{"Error", "TypeError", "RangeError", "SyntaxError", "ReferenceError"} {
		en := errName
		vm.defGlobal(en, makeNative(en, func(v *VM, _ value, args []value) (value, *flow) {
			msg := ""
			if len(args) > 0 && args[0].kind != kindUndefined {
				msg = toString(args[0])
			}
			ms, mf := v.makeString(msg)
			if mf != nil {
				return valUndefined, mf
			}
			o, of := v.newCheckedObject()
			if of != nil {
				return valUndefined, of
			}
			o.set("name", strVal(en))
			o.set("message", ms)
			return objVal(o), nil
		}))
	}

	// Deterministic Date pinned to fixedClock UTC
	dateCtor := makeNative("Date", func(v *VM, thisVal value, args []value) (value, *flow) {
		t := fixedClock
		valid := true
		if len(args) == 1 {
			if args[0].kind == kindString {
				parsed, ok := parseJSDate(args[0].s)
				if !ok {
					valid = false
				} else {
					t = parsed
				}
			} else if args[0].kind == kindBigInt {
				return valUndefined, v.throwTypeError("cannot convert BigInt value to a number")
			} else {
				ms := toNumber(args[0])
				if math.IsNaN(ms) || math.IsInf(ms, 0) {
					valid = false
				} else {
					t = time.UnixMilli(int64(ms)).UTC()
				}
			}
		} else if len(args) >= 2 {
			y := int(toNumber(args[0]))
			m := int(toNumber(args[1])) + 1
			d := 1
			if len(args) >= 3 {
				d = int(toNumber(args[2]))
			}
			t = time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
		}
		if !v.inNew || thisVal.kind != kindObject {
			if !valid {
				return strVal("Invalid Date"), nil
			}
			return strVal(t.Format(time.RFC3339)), nil
		}
		if f := v.chargeAlloc(1); f != nil {
			return valUndefined, f
		}
		return makeDateObject(t, valid), nil
	})
	dateCtor.fn.props.set("now", makeNative("now", func(_ *VM, _ value, _ []value) (value, *flow) {
		return numVal(float64(fixedClock.UnixMilli())), nil
	}))
	dateCtor.fn.props.set("UTC", makeNative("UTC", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) < 1 {
			return valNaN, nil
		}
		y := int(toNumber(args[0]))
		m := 1
		if len(args) >= 2 {
			m = int(toNumber(args[1])) + 1
		}
		d := 1
		if len(args) >= 3 {
			d = int(toNumber(args[2]))
		}
		return numVal(float64(time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).UnixMilli())), nil
	}))
	vm.defGlobal("Date", dateCtor)
}

func parseJSDate(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func makeDateObject(t time.Time, valid bool) value {
	o := newObject()
	o.set("getTime", makeNative("getTime", func(_ *VM, _ value, _ []value) (value, *flow) {
		if !valid {
			return valNaN, nil
		}
		return numVal(float64(t.UnixMilli())), nil
	}))
	o.set("toISOString", makeNative("toISOString", func(v *VM, _ value, _ []value) (value, *flow) {
		if !valid {
			return valUndefined, v.throwError("RangeError", "Invalid time value")
		}
		return strVal(t.UTC().Format("2006-01-02T15:04:05.000Z")), nil
	}))
	o.set("getUTCFullYear", makeNative("getUTCFullYear", func(_ *VM, _ value, _ []value) (value, *flow) {
		if !valid {
			return valNaN, nil
		}
		return numVal(float64(t.UTC().Year())), nil
	}))
	o.set("getUTCMonth", makeNative("getUTCMonth", func(_ *VM, _ value, _ []value) (value, *flow) {
		if !valid {
			return valNaN, nil
		}
		return numVal(float64(t.UTC().Month() - 1)), nil
	}))
	o.set("getUTCDate", makeNative("getUTCDate", func(_ *VM, _ value, _ []value) (value, *flow) {
		if !valid {
			return valNaN, nil
		}
		return numVal(float64(t.UTC().Day())), nil
	}))
	return objVal(o)
}

func unaryMath(fn func(float64) float64) nativeFn {
	return func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valNaN, nil
		}
		if args[0].kind == kindBigInt {
			return valUndefined, v.throwTypeError("cannot convert BigInt value to a number")
		}
		return numVal(fn(toNumber(args[0]))), nil
	}
}

func jsRound(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || x == 0 {
		return x
	}
	return math.Floor(x + 0.5)
}

func digitVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'z':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'Z':
		return int(b-'A') + 10
	default:
		return -1
	}
}

func hasOwnPropNative(v *VM, thisVal value, args []value) (value, *flow) {
	if thisVal.kind == kindUndefined || thisVal.kind == kindNull {
		return valUndefined, v.throwTypeError("cannot convert undefined or null to object")
	}
	if len(args) == 0 {
		return valFalse, nil
	}
	prop := toString(args[0])
	switch thisVal.kind {
	case kindObject:
		_, ok := thisVal.obj.get(prop)
		return boolVal(ok), nil
	case kindArray:
		if prop == "length" {
			return valTrue, nil
		}
		idx, ok := parseArrayIndex(prop)
		return boolVal(ok && idx < len(thisVal.arr.elems)), nil
	case kindString:
		if prop == "length" {
			return valTrue, nil
		}
		idx, ok := parseArrayIndex(prop)
		return boolVal(ok && idx < utf8.RuneCountInString(thisVal.s)), nil
	case kindFunction:
		if thisVal.fn.props != nil {
			_, ok := thisVal.fn.props.get(prop)
			return boolVal(ok), nil
		}
	}
	return valFalse, nil
}

func objToStringNative(_ *VM, thisVal value, _ []value) (value, *flow) {
	switch thisVal.kind {
	case kindUndefined:
		return strVal("[object Undefined]"), nil
	case kindNull:
		return strVal("[object Null]"), nil
	case kindArray:
		return strVal("[object Array]"), nil
	case kindFunction:
		return strVal("[object Function]"), nil
	case kindBool:
		return strVal("[object Boolean]"), nil
	case kindNumber:
		return strVal("[object Number]"), nil
	case kindBigInt:
		return strVal("[object BigInt]"), nil
	case kindString:
		return strVal("[object String]"), nil
	case kindRegex:
		return strVal("[object RegExp]"), nil
	case kindMap:
		return strVal("[object Map]"), nil
	default:
		return strVal("[object Object]"), nil
	}
}

func (vm *VM) objectProtoMethod(prop string) value {
	switch prop {
	case "hasOwnProperty":
		return makeNative("hasOwnProperty", hasOwnPropNative)
	case "toString":
		return makeNative("toString", objToStringNative)
	}
	return valUndefined
}

func (vm *VM) funcProtoMethod(prop string) value {
	switch prop {
	case "call":
		return makeNative("call", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindFunction {
				return valUndefined, v.throwTypeError("Function.prototype.call called on non-function")
			}
			callThis := valUndefined
			var callArgs []value
			if len(args) > 0 {
				callThis = args[0]
				callArgs = args[1:]
			}
			prevNew := v.inNew
			v.inNew = false
			res, f := v.callFunction(thisVal.fn, callThis, callArgs)
			v.inNew = prevNew
			return res, f
		})
	case "apply":
		return makeNative("apply", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindFunction {
				return valUndefined, v.throwTypeError("Function.prototype.apply called on non-function")
			}
			callThis := valUndefined
			if len(args) > 0 {
				callThis = args[0]
			}
			var callArgs []value
			if len(args) > 1 && args[1].kind != kindUndefined && args[1].kind != kindNull {
				if args[1].kind != kindArray {
					return valUndefined, v.throwTypeError("CreateListFromArrayLike called on non-object")
				}
				callArgs = args[1].arr.elems
			}
			prevNew := v.inNew
			v.inNew = false
			res, f := v.callFunction(thisVal.fn, callThis, callArgs)
			v.inNew = prevNew
			return res, f
		})
	}
	return valUndefined
}

func (vm *VM) bigIntProtoMethod(prop string) value {
	switch prop {
	case "toString":
		return makeNative("toString", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindBigInt {
				return valUndefined, v.throwTypeError("BigInt.prototype.toString requires a BigInt")
			}
			radix := 10
			if len(args) > 0 && args[0].kind != kindUndefined {
				radix = int(toInt32(args[0]))
			}
			if radix < 2 || radix > 36 {
				return valUndefined, v.throwError("RangeError", "toString() radix argument must be between 2 and 36")
			}
			return v.makeString(thisVal.bi.Text(radix))
		})
	case "valueOf":
		return makeNative("valueOf", func(v *VM, thisVal value, _ []value) (value, *flow) {
			if thisVal.kind != kindBigInt {
				return valUndefined, v.throwTypeError("BigInt.prototype.valueOf requires a BigInt")
			}
			return thisVal, nil
		})
	}
	return valUndefined
}

func (vm *VM) mapProtoMethod(prop string) value {
	switch prop {
	case "set":
		return makeNative("set", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindMap {
				return valUndefined, v.throwTypeError("Map.prototype.set requires a Map")
			}
			var k, val = valUndefined, valUndefined
			if len(args) > 0 {
				k = args[0]
			}
			if len(args) > 1 {
				val = args[1]
			}
			if f := thisVal.mp.set(v, k, val); f != nil {
				return valUndefined, f
			}
			return thisVal, nil
		})
	case "get":
		return makeNative("get", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindMap {
				return valUndefined, v.throwTypeError("Map.prototype.get requires a Map")
			}
			k := valUndefined
			if len(args) > 0 {
				k = args[0]
			}
			val, _ := thisVal.mp.get(k)
			return val, nil
		})
	case "has":
		return makeNative("has", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindMap {
				return valUndefined, v.throwTypeError("Map.prototype.has requires a Map")
			}
			k := valUndefined
			if len(args) > 0 {
				k = args[0]
			}
			return boolVal(thisVal.mp.findIndex(k) >= 0), nil
		})
	case "delete":
		return makeNative("delete", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindMap {
				return valUndefined, v.throwTypeError("Map.prototype.delete requires a Map")
			}
			k := valUndefined
			if len(args) > 0 {
				k = args[0]
			}
			return boolVal(thisVal.mp.del(k)), nil
		})
	case "clear":
		return makeNative("clear", func(v *VM, thisVal value, _ []value) (value, *flow) {
			if thisVal.kind != kindMap {
				return valUndefined, v.throwTypeError("Map.prototype.clear requires a Map")
			}
			thisVal.mp.clear()
			return valUndefined, nil
		})
	case "keys", "values", "entries":
		mode := prop
		return makeNative(mode, func(v *VM, thisVal value, _ []value) (value, *flow) {
			if thisVal.kind != kindMap {
				return valUndefined, v.throwTypeError("Map.prototype." + mode + " requires a Map")
			}
			var out []value
			for _, entry := range thisVal.mp.entries {
				if !entry.active {
					continue
				}
				switch mode {
				case "keys":
					out = append(out, entry.key)
				case "values":
					out = append(out, entry.val)
				case "entries":
					pair, pf := v.makeArray([]value{entry.key, entry.val})
					if pf != nil {
						return valUndefined, pf
					}
					out = append(out, pair)
				}
			}
			return v.makeArray(out)
		})
	case "forEach":
		return makeNative("forEach", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindMap || len(args) == 0 || args[0].kind != kindFunction {
				return valUndefined, v.throwTypeError("callback must be a function")
			}
			cb := args[0].fn
			thisArg := valUndefined
			if len(args) > 1 {
				thisArg = args[1]
			}
			for i := 0; i < len(thisVal.mp.entries); i++ {
				entry := thisVal.mp.entries[i]
				if !entry.active {
					continue
				}
				if _, f := v.callFunction(cb, thisArg, []value{entry.val, entry.key, thisVal}); f != nil {
					return valUndefined, f
				}
			}
			return valUndefined, nil
		})
	}
	return valUndefined
}

func (vm *VM) numberProtoMethod(prop string) value {
	switch prop {
	case "toFixed":
		return makeNative("toFixed", func(v *VM, thisVal value, args []value) (value, *flow) {
			digits := 0
			if len(args) > 0 && args[0].kind != kindUndefined {
				digits = int(toInt32(args[0]))
			}
			if digits < 0 || digits > 100 {
				return valUndefined, v.throwError("RangeError", "toFixed() digits argument must be between 0 and 100")
			}
			n := toNumber(thisVal)
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return strVal(toString(numVal(n))), nil
			}
			return strVal(strconv.FormatFloat(n, 'f', digits, 64)), nil
		})
	case "toString":
		return makeNative("toString", func(_ *VM, thisVal value, _ []value) (value, *flow) {
			return strVal(toString(thisVal)), nil
		})
	}
	return valUndefined
}

func (vm *VM) stringProtoMethod(prop string) value {
	switch prop {
	case "trim":
		return makeNative("trim", func(v *VM, thisVal value, _ []value) (value, *flow) {
			return v.makeString(strings.TrimSpace(toString(thisVal)))
		})
	case "toLowerCase":
		return makeNative("toLowerCase", func(v *VM, thisVal value, _ []value) (value, *flow) {
			return v.makeString(strings.ToLower(toString(thisVal)))
		})
	case "toUpperCase":
		return makeNative("toUpperCase", func(v *VM, thisVal value, _ []value) (value, *flow) {
			return v.makeString(strings.ToUpper(toString(thisVal)))
		})
	case "indexOf":
		return makeNative("indexOf", func(_ *VM, thisVal value, args []value) (value, *flow) {
			s := toString(thisVal)
			if len(args) == 0 {
				return numVal(-1), nil
			}
			sub := toString(args[0])
			idx := strings.Index(s, sub)
			if idx < 0 {
				return numVal(-1), nil
			}
			return numVal(float64(utf8.RuneCountInString(s[:idx]))), nil
		})
	case "includes":
		return makeNative("includes", func(_ *VM, thisVal value, args []value) (value, *flow) {
			if len(args) == 0 {
				return valFalse, nil
			}
			return boolVal(strings.Contains(toString(thisVal), toString(args[0]))), nil
		})
	case "startsWith":
		return makeNative("startsWith", func(_ *VM, thisVal value, args []value) (value, *flow) {
			if len(args) == 0 {
				return valFalse, nil
			}
			return boolVal(strings.HasPrefix(toString(thisVal), toString(args[0]))), nil
		})
	case "endsWith":
		return makeNative("endsWith", func(_ *VM, thisVal value, args []value) (value, *flow) {
			if len(args) == 0 {
				return valFalse, nil
			}
			return boolVal(strings.HasSuffix(toString(thisVal), toString(args[0]))), nil
		})
	case "slice", "substring":
		isSub := prop == "substring"
		return makeNative(prop, func(v *VM, thisVal value, args []value) (value, *flow) {
			runes := []rune(toString(thisVal))
			l := len(runes)
			start := 0
			if len(args) >= 1 && args[0].kind != kindUndefined {
				start = int(toInt32(args[0]))
			}
			end := l
			if len(args) >= 2 && args[1].kind != kindUndefined {
				end = int(toInt32(args[1]))
			}
			if isSub {
				if start < 0 {
					start = 0
				}
				if end < 0 {
					end = 0
				}
				if start > l {
					start = l
				}
				if end > l {
					end = l
				}
				if start > end {
					start, end = end, start
				}
			} else {
				if start < 0 {
					start = max(l+start, 0)
				} else if start > l {
					start = l
				}
				if end < 0 {
					end = max(l+end, 0)
				} else if end > l {
					end = l
				}
				if end < start {
					end = start
				}
			}
			return v.makeString(string(runes[start:end]))
		})
	case "charAt":
		return makeNative("charAt", func(_ *VM, thisVal value, args []value) (value, *flow) {
			runes := []rune(toString(thisVal))
			idx := 0
			if len(args) > 0 {
				idx = int(toInt32(args[0]))
			}
			if idx < 0 || idx >= len(runes) {
				return strVal(""), nil
			}
			return strVal(string(runes[idx])), nil
		})
	case "charCodeAt":
		return makeNative("charCodeAt", func(_ *VM, thisVal value, args []value) (value, *flow) {
			runes := []rune(toString(thisVal))
			idx := 0
			if len(args) > 0 {
				idx = int(toInt32(args[0]))
			}
			if idx < 0 || idx >= len(runes) {
				return valNaN, nil
			}
			return numVal(float64(runes[idx])), nil
		})
	case "split":
		return makeNative("split", func(v *VM, thisVal value, args []value) (value, *flow) {
			s := toString(thisVal)
			if len(args) == 0 || args[0].kind == kindUndefined {
				return v.makeArray([]value{strVal(s)})
			}
			var parts []string
			if args[0].kind == kindRegex {
				parts = args[0].rx.re.Split(s, maxArrayLen+2)
			} else {
				sep := toString(args[0])
				if sep == "" {
					if utf8.RuneCountInString(s) > maxArrayLen {
						return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
					}
					parts = strings.Split(s, "")
				} else {
					parts = strings.SplitN(s, sep, maxArrayLen+2)
				}
			}
			if len(parts) > maxArrayLen {
				return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
			}
			elems := make([]value, len(parts))
			for i, p := range parts {
				elems[i] = strVal(p)
			}
			return v.makeArray(elems)
		})
	case "replace":
		return makeNative("replace", func(v *VM, thisVal value, args []value) (value, *flow) {
			s := toString(thisVal)
			if len(args) < 2 {
				return v.makeString(s)
			}
			repl := toString(args[1])
			if args[0].kind == kindRegex {
				re := args[0].rx.re
				if strings.Contains(args[0].rx.flags, "g") {
					matches := re.FindAllStringSubmatchIndex(s, -1)
					if len(matches) == 0 {
						return v.makeString(s)
					}
					var out []byte
					last := 0
					for _, m := range matches {
						out = append(out, s[last:m[0]]...)
						out = re.ExpandString(out, repl, s, m)
						if len(out) > maxStringBytes {
							return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
						}
						last = m[1]
					}
					out = append(out, s[last:]...)
					if len(out) > maxStringBytes {
						return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
					}
					return v.makeString(string(out))
				}
				m := re.FindStringSubmatchIndex(s)
				if m == nil {
					return v.makeString(s)
				}
				var out []byte
				out = append(out, s[:m[0]]...)
				out = re.ExpandString(out, repl, s, m)
				out = append(out, s[m[1]:]...)
				if len(out) > maxStringBytes {
					return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
				}
				return v.makeString(string(out))
			}
			needle := toString(args[0])
			idx := strings.Index(s, needle)
			if idx < 0 {
				return v.makeString(s)
			}
			if len(s)-len(needle)+len(repl) > maxStringBytes {
				return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
			}
			return v.makeString(s[:idx] + repl + s[idx+len(needle):])
		})
	case "padStart", "padEnd":
		isStart := prop == "padStart"
		return makeNative(prop, func(v *VM, thisVal value, args []value) (value, *flow) {
			s := toString(thisVal)
			if len(args) == 0 {
				return v.makeString(s)
			}
			targetLen := int(toInt32(args[0]))
			runes := []rune(s)
			if len(runes) >= targetLen {
				return v.makeString(s)
			}
			if targetLen > maxStringBytes {
				return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
			}
			pad := " "
			if len(args) > 1 && args[1].kind != kindUndefined {
				pad = toString(args[1])
			}
			padRunes := []rune(pad)
			if len(padRunes) == 0 {
				return v.makeString(s)
			}
			need := targetLen - len(runes)
			buf := make([]rune, 0, need)
			for len(buf) < need {
				buf = append(buf, padRunes...)
			}
			buf = buf[:need]
			if isStart {
				return v.makeString(string(append(buf, runes...)))
			}
			return v.makeString(string(append(runes, buf...)))
		})
	case "repeat":
		return makeNative("repeat", func(v *VM, thisVal value, args []value) (value, *flow) {
			if len(args) == 0 {
				return strVal(""), nil
			}
			s := toString(thisVal)
			n := int(toInt32(args[0]))
			if n < 0 {
				return valUndefined, v.throwError("RangeError", "invalid repeat count")
			}
			if n == 0 || s == "" {
				return strVal(""), nil
			}
			if n > maxStringBytes/len(s) {
				return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
			}
			return v.makeString(strings.Repeat(s, n))
		})
	}
	return valUndefined
}

func (vm *VM) arrayProtoMethod(prop string) value {
	switch prop {
	case "push":
		return makeNative("push", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindArray {
				return valUndefined, nil
			}
			oldLen := len(thisVal.arr.elems)
			if oldLen+len(args) > maxArrayLen {
				return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
			}
			if (oldLen / 64) != ((oldLen + len(args)) / 64) {
				if f := v.chargeAlloc(1 + len(args)/64); f != nil {
					return valUndefined, f
				}
			}
			thisVal.arr.elems = append(thisVal.arr.elems, args...)
			return numVal(float64(len(thisVal.arr.elems))), nil
		})
	case "pop":
		return makeNative("pop", func(_ *VM, thisVal value, _ []value) (value, *flow) {
			if thisVal.kind != kindArray || len(thisVal.arr.elems) == 0 {
				return valUndefined, nil
			}
			last := thisVal.arr.elems[len(thisVal.arr.elems)-1]
			thisVal.arr.elems = thisVal.arr.elems[:len(thisVal.arr.elems)-1]
			return last, nil
		})
	case "shift":
		return makeNative("shift", func(_ *VM, thisVal value, _ []value) (value, *flow) {
			if thisVal.kind != kindArray || len(thisVal.arr.elems) == 0 {
				return valUndefined, nil
			}
			first := thisVal.arr.elems[0]
			thisVal.arr.elems = thisVal.arr.elems[1:]
			return first, nil
		})
	case "slice":
		return makeNative("slice", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindArray {
				return v.makeArray(nil)
			}
			elems := thisVal.arr.elems
			l := len(elems)
			start, end := 0, l
			if len(args) >= 1 && args[0].kind != kindUndefined {
				start = int(toInt32(args[0]))
				if start < 0 {
					start = max(l+start, 0)
				} else if start > l {
					start = l
				}
			}
			if len(args) >= 2 && args[1].kind != kindUndefined {
				end = int(toInt32(args[1]))
				if end < 0 {
					end = max(l+end, 0)
				} else if end > l {
					end = l
				}
			}
			if end < start {
				end = start
			}
			cp := make([]value, end-start)
			copy(cp, elems[start:end])
			return v.makeArray(cp)
		})
	case "concat":
		return makeNative("concat", func(v *VM, thisVal value, args []value) (value, *flow) {
			total := 0
			if thisVal.kind == kindArray {
				total += len(thisVal.arr.elems)
			}
			for _, a := range args {
				if a.kind == kindArray {
					total += len(a.arr.elems)
				} else {
					total++
				}
				if total > maxArrayLen {
					return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
				}
			}
			out := make([]value, 0, total)
			if thisVal.kind == kindArray {
				out = append(out, thisVal.arr.elems...)
			}
			for _, a := range args {
				if a.kind == kindArray {
					out = append(out, a.arr.elems...)
				} else {
					out = append(out, a)
				}
			}
			return v.makeArray(out)
		})
	case "join":
		return makeNative("join", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindArray {
				return strVal(""), nil
			}
			sep := ","
			if len(args) > 0 && args[0].kind != kindUndefined {
				sep = toString(args[0])
			}
			joined, ok := joinArrayBounded(thisVal.arr, sep, nil, maxStringBytes)
			if !ok {
				return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
			}
			return v.makeString(joined)
		})
	case "indexOf":
		return makeNative("indexOf", func(_ *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindArray || len(args) == 0 {
				return numVal(-1), nil
			}
			for i, el := range thisVal.arr.elems {
				if strictEqual(el, args[0]) {
					return numVal(float64(i)), nil
				}
			}
			return numVal(-1), nil
		})
	case "includes":
		return makeNative("includes", func(_ *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindArray || len(args) == 0 {
				return valFalse, nil
			}
			for _, el := range thisVal.arr.elems {
				if sameValueZero(el, args[0]) {
					return valTrue, nil
				}
			}
			return valFalse, nil
		})
	case "forEach", "map", "filter", "every", "some", "find":
		mode := prop
		return makeNative(mode, func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindArray || len(args) == 0 || args[0].kind != kindFunction {
				return valUndefined, v.throwTypeError("callback must be a function")
			}
			cb := args[0].fn
			thisArg := valUndefined
			if len(args) > 1 {
				thisArg = args[1]
			}
			var out []value
			for i := 0; i < len(thisVal.arr.elems); i++ {
				el := thisVal.arr.elems[i]
				res, f := v.callFunction(cb, thisArg, []value{el, numVal(float64(i)), thisVal})
				if f != nil {
					return valUndefined, f
				}
				switch mode {
				case "map":
					if len(out) >= maxArrayLen {
						return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
					}
					out = append(out, res)
				case "filter":
					if toBool(res) {
						if len(out) >= maxArrayLen {
							return valUndefined, v.throwError("RangeError", "maximum array length exceeded")
						}
						out = append(out, el)
					}
				case "every":
					if !toBool(res) {
						return valFalse, nil
					}
				case "some":
					if toBool(res) {
						return valTrue, nil
					}
				case "find":
					if toBool(res) {
						return el, nil
					}
				}
			}
			switch mode {
			case "map", "filter":
				return v.makeArray(out)
			case "every":
				return valTrue, nil
			case "some":
				return valFalse, nil
			default:
				return valUndefined, nil
			}
		})
	case "reduce":
		return makeNative("reduce", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindArray || len(args) == 0 || args[0].kind != kindFunction {
				return valUndefined, v.throwTypeError("callback must be a function")
			}
			cb := args[0].fn
			elems := thisVal.arr.elems
			var acc value
			start := 0
			if len(args) >= 2 {
				acc = args[1]
			} else {
				if len(elems) == 0 {
					return valUndefined, v.throwTypeError("reduce of empty array with no initial value")
				}
				acc = elems[0]
				start = 1
			}
			for i := start; i < len(elems); i++ {
				var f *flow
				acc, f = v.callFunction(cb, valUndefined, []value{acc, elems[i], numVal(float64(i)), thisVal})
				if f != nil {
					return valUndefined, f
				}
			}
			return acc, nil
		})
	case "sort":
		return makeNative("sort", func(v *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindArray {
				return thisVal, nil
			}
			elems := thisVal.arr.elems
			if len(args) > 0 && args[0].kind == kindFunction {
				cb := args[0].fn
				var sortFlow *flow
				sort.SliceStable(elems, func(i, j int) bool {
					if sortFlow != nil {
						return false
					}
					res, f := v.callFunction(cb, valUndefined, []value{elems[i], elems[j]})
					if f != nil {
						sortFlow = f
						return false
					}
					return toNumber(res) < 0
				})
				if sortFlow != nil {
					return valUndefined, sortFlow
				}
			} else {
				sort.SliceStable(elems, func(i, j int) bool {
					return toString(elems[i]) < toString(elems[j])
				})
			}
			return thisVal, nil
		})
	}
	return valUndefined
}

func compileJSRegex(vm *VM, pattern, flags string) (*jsRegex, *flow) {
	if len(pattern) > maxStringBytes {
		return nil, vm.throwError("RangeError", "regular expression pattern too large")
	}
	if f := vm.chargeAlloc(1); f != nil {
		return nil, f
	}
	prefix := ""
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case 'i':
			prefix += "(?i)"
		case 'm':
			prefix += "(?m)"
		case 's':
			prefix += "(?s)"
		case 'g', 'u':
		default:
			return nil, vm.throwError("SyntaxError", "invalid regex flag")
		}
	}
	re, err := regexp.Compile(prefix + pattern)
	if err != nil {
		return nil, vm.throwError("SyntaxError", "unsupported or invalid regular expression")
	}
	return &jsRegex{pattern: pattern, flags: flags, re: re}, nil
}

func (vm *VM) regexProtoMethod(prop string) value {
	switch prop {
	case "test":
		return makeNative("test", func(_ *VM, thisVal value, args []value) (value, *flow) {
			if thisVal.kind != kindRegex {
				return valFalse, nil
			}
			s := "undefined"
			if len(args) > 0 {
				s = toString(args[0])
			}
			return boolVal(thisVal.rx.re.MatchString(s)), nil
		})
	}
	return valUndefined
}

// JSON conversion helpers

var errJSONLimit = errors.New("JSON value exceeds VM resource bounds")

func (vm *VM) parseJSONValueChecked(data []byte) (value, error) {
	if len(data) > maxStringBytes*4 {
		return valUndefined, errJSONLimit
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := vm.decodeJSONTokenChecked(dec, 0)
	if err != nil {
		return valUndefined, err
	}
	if _, err := dec.Token(); err == nil {
		return valUndefined, errors.New("trailing json tokens")
	}
	return v, nil
}

func (vm *VM) decodeJSONTokenChecked(dec *json.Decoder, depth int) (value, error) {
	if depth > maxJSONDepth {
		return valUndefined, errJSONLimit
	}
	tok, err := dec.Token()
	if err != nil {
		return valUndefined, err
	}
	switch t := tok.(type) {
	case nil:
		return valNull, nil
	case bool:
		return boolVal(t), nil
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return valUndefined, err
		}
		return numVal(f), nil
	case string:
		v, f := vm.makeString(t)
		if f != nil {
			return valUndefined, errJSONLimit
		}
		return v, nil
	case json.Delim:
		switch t {
		case '[':
			var elems []value
			for dec.More() {
				if len(elems) >= maxArrayLen {
					return valUndefined, errJSONLimit
				}
				el, err := vm.decodeJSONTokenChecked(dec, depth+1)
				if err != nil {
					return valUndefined, err
				}
				elems = append(elems, el)
			}
			if _, err := dec.Token(); err != nil {
				return valUndefined, err
			}
			arr, af := vm.makeArray(elems)
			if af != nil {
				return valUndefined, errJSONLimit
			}
			return arr, nil
		case '{':
			o, of := vm.newCheckedObject()
			if of != nil {
				return valUndefined, errJSONLimit
			}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return valUndefined, err
				}
				key, ok := kt.(string)
				if !ok {
					return valUndefined, errors.New("expected string key")
				}
				if len(key) > maxStringBytes {
					return valUndefined, errJSONLimit
				}
				val, err := vm.decodeJSONTokenChecked(dec, depth+1)
				if err != nil {
					return valUndefined, err
				}
				if sf := vm.setObjProp(o, key, val); sf != nil {
					return valUndefined, errJSONLimit
				}
			}
			if _, err := dec.Token(); err != nil {
				return valUndefined, err
			}
			return objVal(o), nil
		}
	}
	return valUndefined, errors.New("unexpected json token")
}

func stringifyValue(vm *VM, v value) (string, bool, *flow) {
	seenObj := map[*jsObject]bool{}
	seenArr := map[*jsArray]bool{}
	var buf bytes.Buffer

	var write func(val value) (bool, *flow)
	write = func(val value) (bool, *flow) {
		if buf.Len() > maxStringBytes {
			return false, vm.throwError("RangeError", "maximum string length exceeded")
		}
		switch val.kind {
		case kindUndefined, kindFunction:
			return false, nil
		case kindBigInt:
			return false, vm.throwTypeError("Do not know how to serialize a BigInt")
		case kindNull:
			buf.WriteString("null")
			return true, nil
		case kindBool:
			if val.b {
				buf.WriteString("true")
			} else {
				buf.WriteString("false")
			}
			return true, nil
		case kindNumber:
			if math.IsNaN(val.n) || math.IsInf(val.n, 0) {
				buf.WriteString("null")
				return true, nil
			}
			buf.WriteString(formatJSNumber(val.n))
			return true, nil
		case kindString:
			if buf.Len()+len(val.s) > maxStringBytes {
				return false, vm.throwError("RangeError", "maximum string length exceeded")
			}
			enc, _ := json.Marshal(val.s)
			if buf.Len()+len(enc) > maxStringBytes {
				return false, vm.throwError("RangeError", "maximum string length exceeded")
			}
			buf.Write(enc)
			return true, nil
		case kindArray:
			if seenArr[val.arr] {
				return false, vm.throwTypeError("converting circular structure to JSON")
			}
			seenArr[val.arr] = true
			defer delete(seenArr, val.arr)
			buf.WriteByte('[')
			for i, el := range val.arr.elems {
				if i > 0 {
					buf.WriteByte(',')
				}
				mark := buf.Len()
				ok, f := write(el)
				if f != nil {
					return false, f
				}
				if !ok {
					buf.Truncate(mark)
					buf.WriteString("null")
				}
			}
			buf.WriteByte(']')
			if buf.Len() > maxStringBytes {
				return false, vm.throwError("RangeError", "maximum string length exceeded")
			}
			return true, nil
		case kindObject:
			if seenObj[val.obj] {
				return false, vm.throwTypeError("converting circular structure to JSON")
			}
			seenObj[val.obj] = true
			defer delete(seenObj, val.obj)
			buf.WriteByte('{')
			written := 0
			for _, k := range val.obj.order {
				pv := val.obj.props[k]
				if pv.kind == kindUndefined || pv.kind == kindFunction {
					continue
				}
				if written > 0 {
					buf.WriteByte(',')
				}
				keyBytes, _ := json.Marshal(k)
				if buf.Len()+len(keyBytes)+1 > maxStringBytes {
					return false, vm.throwError("RangeError", "maximum string length exceeded")
				}
				buf.Write(keyBytes)
				buf.WriteByte(':')
				_, f := write(pv)
				if f != nil {
					return false, f
				}
				written++
			}
			buf.WriteByte('}')
			if buf.Len() > maxStringBytes {
				return false, vm.throwError("RangeError", "maximum string length exceeded")
			}
			return true, nil
		default:
			buf.WriteString("{}")
			return true, nil
		}
	}

	ok, f := write(v)
	if f != nil || !ok {
		return "", false, f
	}
	if buf.Len() > maxStringBytes {
		return "", false, vm.throwError("RangeError", "maximum string length exceeded")
	}
	return buf.String(), true, nil
}

