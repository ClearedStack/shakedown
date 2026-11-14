package js

import (
	"context"
	"errors"
	"strings"

	"github.com/ClearedStack/shakedown/internal/htmlparse"
)

const (
	maxDOMNodes       = 8192
	maxDOMDepth       = 256
	maxDOMListeners   = 512
	maxDOMTimers      = 256
	maxStorageEntries = 128
	maxStorageBytes   = 64 << 10
)

type domNodeKind int

const (
	domDocument domNodeKind = iota
	domElement
	domText
)

type domListener struct {
	typ string
	fn  *jsFunc
}

type domNode struct {
	dom          *DOM
	kind         domNodeKind
	tag          string
	text         string
	attrs        map[string]string
	value        string
	defaultValue string
	parent       *domNode
	children     []*domNode
	listeners    []domListener
	wrapper      *jsObject
}

type domTimer struct {
	id       int
	fn       *jsFunc
	canceled bool
}

type domStorage struct {
	data  map[string]string
	order []string
	bytes int
	obj   *jsObject
}

// DOM is a bounded, deterministic, offline browser environment attached to a VM.
type DOM struct {
	vm            *VM
	doc           *domNode
	nodeCount     int
	listenerCount int
	timers        []domTimer
	nextTimerID   int
	localStore    *domStorage
	sessionStore  *domStorage
}

// DOMElement inspects an HTML element in a DOM instance from Go.
type DOMElement struct {
	node *domNode
}

// ID returns the element's id attribute, or "".
func (e *DOMElement) ID() string {
	if e == nil || e.node == nil {
		return ""
	}
	return e.node.attrs["id"]
}

// TagName returns the upper-case HTML tag name (such as "LI" or "SELECT").
func (e *DOMElement) TagName() string {
	if e == nil || e.node == nil {
		return ""
	}
	return strings.ToUpper(e.node.tag)
}

// Value returns the current form control value string.
func (e *DOMElement) Value() string {
	if e == nil || e.node == nil {
		return ""
	}
	return e.node.value
}

// TextContent returns the concatenated text content of the element and its descendants.
func (e *DOMElement) TextContent() string {
	if e == nil || e.node == nil {
		return ""
	}
	return e.node.collectText()
}

// ClassName returns the element's class attribute, or "".
func (e *DOMElement) ClassName() string {
	if e == nil || e.node == nil {
		return ""
	}
	return e.node.attrs["class"]
}

// Hidden reports whether the element has the boolean hidden attribute set.
func (e *DOMElement) Hidden() bool {
	if e == nil || e.node == nil {
		return false
	}
	_, ok := e.node.attrs["hidden"]
	return ok
}

// Attr returns a lower-case HTML attribute and whether it is present.
func (e *DOMElement) Attr(name string) (string, bool) {
	if e == nil || e.node == nil {
		return "", false
	}
	v, ok := e.node.attrs[strings.ToLower(name)]
	return v, ok
}

// Children returns the element's child elements in document order.
func (e *DOMElement) Children() []*DOMElement {
	if e == nil || e.node == nil {
		return nil
	}
	var out []*DOMElement
	for _, ch := range e.node.children {
		if ch.kind == domElement {
			out = append(out, &DOMElement{node: ch})
		}
	}
	return out
}

// AttachDOM builds a bounded in-memory DOM from doc and binds window, document,
// setTimeout, clearTimeout, localStorage, sessionStorage, and FileReader on vm.
func (vm *VM) AttachDOM(doc *htmlparse.Node) (*DOM, error) {
	d := &DOM{
		vm:           vm,
		localStore:   newDOMStorage(),
		sessionStore: newDOMStorage(),
	}
	var err error
	if doc == nil {
		d.doc = &domNode{dom: d, kind: domDocument, tag: "#document"}
		d.nodeCount = 1
	} else {
		d.doc, err = d.buildFromHTML(doc, 0)
		if err != nil {
			return nil, err
		}
		if d.doc.kind != domDocument {
			root := &domNode{dom: d, kind: domDocument, tag: "#document"}
			d.doc.parent = root
			root.children = []*domNode{d.doc}
			d.doc = root
			d.nodeCount++
		}
	}

	vm.defGlobal("window", objVal(vm.globalObj))
	vm.defGlobal("document", d.doc.jsValue(vm))
	vm.defGlobal("localStorage", objVal(d.localStore.obj))
	vm.defGlobal("sessionStorage", objVal(d.sessionStore.obj))

	vm.defGlobal("setTimeout", makeNative("setTimeout", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind != kindFunction {
			return valUndefined, v.throwTypeError("setTimeout requires a function callback")
		}
		if len(d.timers) >= maxDOMTimers {
			return valUndefined, v.throwError("RangeError", "maximum timer queue length exceeded")
		}
		d.nextTimerID++
		id := d.nextTimerID
		d.timers = append(d.timers, domTimer{id: id, fn: args[0].fn})
		return numVal(float64(id)), nil
	}))

	vm.defGlobal("clearTimeout", makeNative("clearTimeout", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valUndefined, nil
		}
		target := int(toNumber(args[0]))
		for i := range d.timers {
			if d.timers[i].id == target {
				d.timers[i].canceled = true
			}
		}
		return valUndefined, nil
	}))

	vm.defGlobal("FileReader", makeNative("FileReader", func(v *VM, thisVal value, _ []value) (value, *flow) {
		var reader *jsObject
		if v.inNew && thisVal.kind == kindObject {
			reader = thisVal.obj
		} else {
			var f *flow
			reader, f = v.newCheckedObject()
			if f != nil {
				return valUndefined, f
			}
		}
		var loadListeners []*jsFunc
		reader.set("readyState", numVal(0))
		reader.set("result", valNull)
		reader.set("addEventListener", makeNative("addEventListener", func(vm2 *VM, _ value, a []value) (value, *flow) {
			if len(a) >= 2 && toString(a[0]) == "load" && a[1].kind == kindFunction {
				if len(loadListeners) >= maxDOMListeners {
					return valUndefined, vm2.throwError("RangeError", "maximum listener count exceeded")
				}
				loadListeners = append(loadListeners, a[1].fn)
			}
			return valUndefined, nil
		}))
		reader.set("readAsText", makeNative("readAsText", func(vm2 *VM, _ value, a []value) (value, *flow) {
			text := ""
			if len(a) > 0 {
				if a[0].kind == kindString {
					text = a[0].s
				} else if a[0].kind == kindObject {
					if tv, ok := a[0].obj.get("text"); ok && tv.kind == kindString {
						text = tv.s
					}
				}
			}
			sv, sf := vm2.makeString(text)
			if sf != nil {
				return valUndefined, sf
			}
			reader.set("readyState", numVal(2))
			reader.set("result", sv)
			for _, fn := range loadListeners {
				ev, ef := vm2.newEventObject("load", objVal(reader), objVal(reader))
				if ef != nil {
					return valUndefined, ef
				}
				if _, cf := vm2.callFunction(fn, objVal(reader), []value{ev}); cf != nil {
					return valUndefined, cf
				}
			}
			return valUndefined, nil
		}))
		return objVal(reader), nil
	}))

	return d, nil
}

func newDOMStorage() *domStorage {
	st := &domStorage{
		data: make(map[string]string),
		obj:  newObject(),
	}
	st.obj.set("length", numVal(0))
	st.obj.set("getItem", makeNative("getItem", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valNull, nil
		}
		val, ok := st.data[toString(args[0])]
		if !ok {
			return valNull, nil
		}
		return v.makeString(val)
	}))
	st.obj.set("setItem", makeNative("setItem", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) < 2 {
			return valUndefined, v.throwTypeError("setItem requires key and value")
		}
		k := toString(args[0])
		val := toString(args[1])
		old, exists := st.data[k]
		if !exists && len(st.order) >= maxStorageEntries {
			return valUndefined, v.throwError("QuotaExceededError", "storage entry limit exceeded")
		}
		delta := len(val)
		if exists {
			delta -= len(old)
		} else {
			delta += len(k)
		}
		if st.bytes+delta > maxStorageBytes {
			return valUndefined, v.throwError("QuotaExceededError", "storage byte quota exceeded")
		}
		if !exists {
			st.order = append(st.order, k)
		}
		st.data[k] = val
		st.bytes += delta
		st.obj.set("length", numVal(float64(len(st.order))))
		return valUndefined, nil
	}))
	st.obj.set("removeItem", makeNative("removeItem", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valUndefined, nil
		}
		k := toString(args[0])
		old, exists := st.data[k]
		if !exists {
			return valUndefined, nil
		}
		delete(st.data, k)
		st.bytes -= len(k) + len(old)
		for i, item := range st.order {
			if item == k {
				st.order = append(st.order[:i], st.order[i+1:]...)
				break
			}
		}
		st.obj.set("length", numVal(float64(len(st.order))))
		return valUndefined, nil
	}))
	st.obj.set("clear", makeNative("clear", func(_ *VM, _ value, _ []value) (value, *flow) {
		st.data = make(map[string]string)
		st.order = nil
		st.bytes = 0
		st.obj.set("length", numVal(0))
		return valUndefined, nil
	}))
	st.obj.set("key", makeNative("key", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 {
			return valNull, nil
		}
		idx := int(toNumber(args[0]))
		if idx < 0 || idx >= len(st.order) {
			return valNull, nil
		}
		return v.makeString(st.order[idx])
	}))
	return st
}

func (d *DOM) buildFromHTML(n *htmlparse.Node, depth int) (*domNode, error) {
	if depth > maxDOMDepth {
		return nil, errors.New("dom depth limit exceeded")
	}
	d.nodeCount++
	if d.nodeCount > maxDOMNodes {
		return nil, errors.New("dom node limit exceeded")
	}

	var dn *domNode
	switch n.Type {
	case htmlparse.DocumentNode:
		dn = &domNode{dom: d, kind: domDocument, tag: "#document"}
	case htmlparse.TextNode:
		return &domNode{dom: d, kind: domText, text: n.Data}, nil
	case htmlparse.ElementNode:
		attrs := make(map[string]string, len(n.Attr))
		for _, a := range n.Attr {
			attrs[strings.ToLower(a.Key)] = a.Val
		}
		dn = &domNode{
			dom:   d,
			kind:  domElement,
			tag:   strings.ToLower(n.Data),
			attrs: attrs,
		}
	default:
		return nil, errors.New("unsupported html node type")
	}

	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		child, err := d.buildFromHTML(ch, depth+1)
		if err != nil {
			return nil, err
		}
		child.parent = dn
		dn.children = append(dn.children, child)
	}

	if dn.kind == domElement {
		switch dn.tag {
		case "input":
			dn.defaultValue = dn.attrs["value"]
			dn.value = dn.defaultValue
		case "textarea":
			dn.defaultValue = dn.collectText()
			dn.value = dn.defaultValue
		case "select":
			dn.defaultValue = selectInitialValue(dn)
			dn.value = dn.defaultValue
		}
	}
	return dn, nil
}

func selectInitialValue(sel *domNode) string {
	var options []*domNode
	var walk func(*domNode)
	walk = func(cur *domNode) {
		for _, ch := range cur.children {
			if ch.kind == domElement {
				if ch.tag == "option" {
					options = append(options, ch)
				} else {
					walk(ch)
				}
			}
		}
	}
	walk(sel)
	if len(options) == 0 {
		return ""
	}
	chosen := options[0]
	for _, opt := range options {
		if _, hasSel := opt.attrs["selected"]; hasSel {
			chosen = opt
			break
		}
	}
	if v, hasVal := chosen.attrs["value"]; hasVal {
		return v
	}
	return strings.TrimSpace(chosen.collectText())
}

func (n *domNode) collectText() string {
	if n.kind == domText {
		return n.text
	}
	var b strings.Builder
	var walk func(*domNode)
	walk = func(cur *domNode) {
		if cur.kind == domText {
			b.WriteString(cur.text)
			return
		}
		for _, ch := range cur.children {
			walk(ch)
		}
	}
	walk(n)
	return b.String()
}

func (n *domNode) findByID(id string) *domNode {
	if id == "" || n == nil {
		return nil
	}
	if n.kind == domElement && n.attrs["id"] == id {
		return n
	}
	for _, ch := range n.children {
		if found := ch.findByID(id); found != nil {
			return found
		}
	}
	return nil
}

func (n *domNode) findByTag(tag string) *domNode {
	if n == nil {
		return nil
	}
	if n.kind == domElement && n.tag == tag {
		return n
	}
	for _, ch := range n.children {
		if found := ch.findByTag(tag); found != nil {
			return found
		}
	}
	return nil
}

func (n *domNode) detachFromParent() {
	if n.parent == nil {
		return
	}
	siblings := n.parent.children
	for i, ch := range siblings {
		if ch == n {
			n.parent.children = append(siblings[:i], siblings[i+1:]...)
			break
		}
	}
	n.parent = nil
}

func (n *domNode) isAncestorOf(other *domNode) bool {
	for cur := other; cur != nil; cur = cur.parent {
		if cur == n {
			return true
		}
	}
	return false
}

func (n *domNode) jsValue(vm *VM) value {
	if n == nil {
		return valNull
	}
	if n.wrapper != nil {
		return objVal(n.wrapper)
	}
	obj := &jsObject{
		props: make(map[string]value),
		dom:   n,
	}
	n.wrapper = obj
	n.installMethods(vm, obj)
	return objVal(obj)
}

func (n *domNode) installMethods(_ *VM, obj *jsObject) {
	obj.set("addEventListener", makeNative("addEventListener", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) < 2 || args[1].kind != kindFunction {
			return valUndefined, nil
		}
		typ := toString(args[0])
		fn := args[1].fn
		for _, existing := range n.listeners {
			if existing.typ == typ && existing.fn == fn {
				return valUndefined, nil
			}
		}
		if n.dom.listenerCount >= maxDOMListeners {
			return valUndefined, v.throwError("RangeError", "maximum DOM listener count exceeded")
		}
		n.dom.listenerCount++
		n.listeners = append(n.listeners, domListener{typ: typ, fn: fn})
		return valUndefined, nil
	}))

	obj.set("removeEventListener", makeNative("removeEventListener", func(_ *VM, _ value, args []value) (value, *flow) {
		if len(args) < 2 || args[1].kind != kindFunction {
			return valUndefined, nil
		}
		typ := toString(args[0])
		fn := args[1].fn
		for i, existing := range n.listeners {
			if existing.typ == typ && existing.fn == fn {
				n.listeners = append(n.listeners[:i], n.listeners[i+1:]...)
				n.dom.listenerCount--
				break
			}
		}
		return valUndefined, nil
	}))

	obj.set("dispatchEvent", makeNative("dispatchEvent", func(v *VM, _ value, args []value) (value, *flow) {
		if len(args) == 0 || args[0].kind != kindObject {
			return valUndefined, v.throwTypeError("dispatchEvent requires an event object")
		}
		typVal, _ := args[0].obj.get("type")
		typ := toString(typVal)
		if typ == "" {
			return valFalse, nil
		}
		notPrevented, f := n.dom.dispatchNodeEvent(v, n, typ, true)
		if f != nil {
			return valUndefined, f
		}
		return boolVal(notPrevented), nil
	}))

	if n.kind == domDocument {
		obj.set("getElementById", makeNative("getElementById", func(v *VM, _ value, args []value) (value, *flow) {
			if len(args) == 0 {
				return valNull, nil
			}
			found := n.findByID(toString(args[0]))
			if found == nil {
				return valNull, nil
			}
			return found.jsValue(v), nil
		}))

		obj.set("createElement", makeNative("createElement", func(v *VM, _ value, args []value) (value, *flow) {
			tag := ""
			if len(args) > 0 {
				tag = strings.ToLower(strings.TrimSpace(toString(args[0])))
			}
			if tag == "" {
				return valUndefined, v.throwError("Error", "invalid tag name")
			}
			if n.dom.nodeCount >= maxDOMNodes {
				return valUndefined, v.throwError("RangeError", "maximum DOM node count exceeded")
			}
			if f := v.chargeAlloc(1); f != nil {
				return valUndefined, f
			}
			n.dom.nodeCount++
			elem := &domNode{
				dom:   n.dom,
				kind:  domElement,
				tag:   tag,
				attrs: make(map[string]string),
			}
			return elem.jsValue(v), nil
		}))

		obj.set("createTextNode", makeNative("createTextNode", func(v *VM, _ value, args []value) (value, *flow) {
			txt := ""
			if len(args) > 0 {
				txt = toString(args[0])
			}
			if len(txt) > maxStringBytes {
				return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
			}
			if n.dom.nodeCount >= maxDOMNodes {
				return valUndefined, v.throwError("RangeError", "maximum DOM node count exceeded")
			}
			if f := v.chargeAlloc(1); f != nil {
				return valUndefined, f
			}
			n.dom.nodeCount++
			tn := &domNode{
				dom:  n.dom,
				kind: domText,
				text: txt,
			}
			return tn.jsValue(v), nil
		}))
		return
	}

	if n.kind == domElement {
		obj.set("getAttribute", makeNative("getAttribute", func(v *VM, _ value, args []value) (value, *flow) {
			if len(args) == 0 {
				return valNull, nil
			}
			val, ok := n.attrs[strings.ToLower(toString(args[0]))]
			if !ok {
				return valNull, nil
			}
			return v.makeString(val)
		}))

		obj.set("setAttribute", makeNative("setAttribute", func(v *VM, _ value, args []value) (value, *flow) {
			if len(args) < 2 {
				return valUndefined, v.throwTypeError("setAttribute requires name and value")
			}
			k := strings.ToLower(toString(args[0]))
			val := toString(args[1])
			if len(k) > maxStringBytes || len(val) > maxStringBytes {
				return valUndefined, v.throwError("RangeError", "maximum string length exceeded")
			}
			if _, exists := n.attrs[k]; !exists && len(n.attrs) >= maxObjectProps {
				return valUndefined, v.throwError("RangeError", "maximum attribute count exceeded")
			}
			n.attrs[k] = val
			return valUndefined, nil
		}))

		obj.set("removeAttribute", makeNative("removeAttribute", func(_ *VM, _ value, args []value) (value, *flow) {
			if len(args) > 0 {
				delete(n.attrs, strings.ToLower(toString(args[0])))
			}
			return valUndefined, nil
		}))

		obj.set("hasAttribute", makeNative("hasAttribute", func(_ *VM, _ value, args []value) (value, *flow) {
			if len(args) == 0 {
				return valFalse, nil
			}
			_, ok := n.attrs[strings.ToLower(toString(args[0]))]
			return boolVal(ok), nil
		}))

		obj.set("appendChild", makeNative("appendChild", func(v *VM, _ value, args []value) (value, *flow) {
			if len(args) == 0 || args[0].kind != kindObject || args[0].obj.dom == nil {
				return valUndefined, v.throwTypeError("appendChild requires a DOM node")
			}
			child := args[0].obj.dom
			if child.dom != n.dom || child.kind == domDocument || child.isAncestorOf(n) {
				return valUndefined, v.throwError("HierarchyRequestError", "invalid DOM hierarchy")
			}
			child.detachFromParent()
			child.parent = n
			n.children = append(n.children, child)
			return args[0], nil
		}))

		obj.set("removeChild", makeNative("removeChild", func(v *VM, _ value, args []value) (value, *flow) {
			if len(args) == 0 || args[0].kind != kindObject || args[0].obj.dom == nil {
				return valUndefined, v.throwTypeError("removeChild requires a DOM node")
			}
			child := args[0].obj.dom
			for i, ch := range n.children {
				if ch == child {
					n.children = append(n.children[:i], n.children[i+1:]...)
					child.parent = nil
					return args[0], nil
				}
			}
			return valUndefined, v.throwError("NotFoundError", "the node to be removed is not a child of this node")
		}))
	}
}

func (vm *VM) getDOMProp(n *domNode, prop string) (value, bool, *flow) {
	switch prop {
	case "nodeType":
		switch n.kind {
		case domDocument:
			return numVal(9), true, nil
		case domElement:
			return numVal(1), true, nil
		default:
			return numVal(3), true, nil
		}
	case "nodeName":
		switch n.kind {
		case domDocument:
			return strVal("#document"), true, nil
		case domElement:
			return strVal(strings.ToUpper(n.tag)), true, nil
		default:
			return strVal("#text"), true, nil
		}
	case "firstChild":
		if len(n.children) == 0 {
			return valNull, true, nil
		}
		return n.children[0].jsValue(vm), true, nil
	case "lastChild":
		if len(n.children) == 0 {
			return valNull, true, nil
		}
		return n.children[len(n.children)-1].jsValue(vm), true, nil
	case "parentNode":
		if n.parent == nil {
			return valNull, true, nil
		}
		return n.parent.jsValue(vm), true, nil
	case "parentElement":
		if n.parent == nil || n.parent.kind != domElement {
			return valNull, true, nil
		}
		return n.parent.jsValue(vm), true, nil
	case "nextSibling":
		if n.parent == nil {
			return valNull, true, nil
		}
		for i, ch := range n.parent.children {
			if ch == n && i+1 < len(n.parent.children) {
				return n.parent.children[i+1].jsValue(vm), true, nil
			}
		}
		return valNull, true, nil
	case "textContent":
		v, f := vm.makeString(n.collectText())
		return v, true, f
	case "childNodes":
		elems := make([]value, 0, len(n.children))
		for _, ch := range n.children {
			elems = append(elems, ch.jsValue(vm))
		}
		arr, f := vm.makeArray(elems)
		return arr, true, f
	case "children":
		var elems []value
		for _, ch := range n.children {
			if ch.kind == domElement {
				elems = append(elems, ch.jsValue(vm))
			}
		}
		arr, f := vm.makeArray(elems)
		return arr, true, f
	}

	switch n.kind {
	case domDocument:
		switch prop {
		case "readyState":
			return strVal("complete"), true, nil
		case "documentElement":
			return n.findByTag("html").jsValue(vm), true, nil
		case "head":
			return n.findByTag("head").jsValue(vm), true, nil
		case "body":
			return n.findByTag("body").jsValue(vm), true, nil
		}
	case domElement:
		switch prop {
		case "tagName":
			return strVal(strings.ToUpper(n.tag)), true, nil
		case "id":
			return strVal(n.attrs["id"]), true, nil
		case "className":
			return strVal(n.attrs["class"]), true, nil
		case "hidden":
			_, ok := n.attrs["hidden"]
			return boolVal(ok), true, nil
		case "value":
			v, f := vm.makeString(n.value)
			return v, true, f
		case "defaultValue":
			v, f := vm.makeString(n.defaultValue)
			return v, true, f
		case "files":
			arr, f := vm.makeArray(nil)
			return arr, true, f
		}
	case domText:
		switch prop {
		case "nodeValue", "data":
			v, f := vm.makeString(n.text)
			return v, true, f
		}
	}
	return valUndefined, false, nil
}

func (vm *VM) setDOMProp(n *domNode, prop string, val value) (bool, *flow) {
	if prop == "textContent" {
		s := toString(val)
		if len(s) > maxStringBytes {
			return true, vm.throwError("RangeError", "maximum string length exceeded")
		}
		if n.kind == domText {
			n.text = s
			return true, nil
		}
		for _, ch := range n.children {
			ch.parent = nil
		}
		n.children = nil
		if s != "" {
			if n.dom.nodeCount >= maxDOMNodes {
				return true, vm.throwError("RangeError", "maximum DOM node count exceeded")
			}
			n.dom.nodeCount++
			tn := &domNode{
				dom:    n.dom,
				kind:   domText,
				text:   s,
				parent: n,
			}
			n.children = []*domNode{tn}
		}
		return true, nil
	}

	if n.kind == domText && (prop == "nodeValue" || prop == "data") {
		s := toString(val)
		if len(s) > maxStringBytes {
			return true, vm.throwError("RangeError", "maximum string length exceeded")
		}
		n.text = s
		return true, nil
	}

	if n.kind == domElement {
		switch prop {
		case "id":
			s := toString(val)
			if len(s) > maxStringBytes {
				return true, vm.throwError("RangeError", "maximum string length exceeded")
			}
			n.attrs["id"] = s
			return true, nil
		case "className":
			s := toString(val)
			if len(s) > maxStringBytes {
				return true, vm.throwError("RangeError", "maximum string length exceeded")
			}
			n.attrs["class"] = s
			return true, nil
		case "hidden":
			if toBool(val) {
				n.attrs["hidden"] = ""
			} else {
				delete(n.attrs, "hidden")
			}
			return true, nil
		case "value":
			s := toString(val)
			if len(s) > maxStringBytes {
				return true, vm.throwError("RangeError", "maximum string length exceeded")
			}
			n.value = s
			return true, nil
		case "defaultValue":
			s := toString(val)
			if len(s) > maxStringBytes {
				return true, vm.throwError("RangeError", "maximum string length exceeded")
			}
			n.defaultValue = s
			return true, nil
		case "nodeType", "nodeName", "tagName", "firstChild", "lastChild", "parentNode", "parentElement", "nextSibling", "childNodes", "children":
			return true, nil
		}
	}

	if n.kind == domDocument {
		switch prop {
		case "readyState", "documentElement", "head", "body", "nodeType", "nodeName", "firstChild", "lastChild":
			return true, nil
		}
	}

	return false, nil
}

func (vm *VM) newEventObject(typ string, target, currentTarget value) (value, *flow) {
	ev, f := vm.newCheckedObject()
	if f != nil {
		return valUndefined, f
	}
	ev.set("type", strVal(typ))
	ev.set("target", target)
	ev.set("currentTarget", currentTarget)
	ev.set("defaultPrevented", valFalse)
	ev.set("cancelBubble", valFalse)
	ev.set("preventDefault", makeNative("preventDefault", func(_ *VM, _ value, _ []value) (value, *flow) {
		ev.set("defaultPrevented", valTrue)
		return valUndefined, nil
	}))
	ev.set("stopPropagation", makeNative("stopPropagation", func(_ *VM, _ value, _ []value) (value, *flow) {
		ev.set("cancelBubble", valTrue)
		return valUndefined, nil
	}))
	return objVal(ev), nil
}

func (d *DOM) dispatchNodeEvent(vm *VM, target *domNode, typ string, bubbles bool) (bool, *flow) {
	targetVal := target.jsValue(vm)
	evVal, f := vm.newEventObject(typ, targetVal, targetVal)
	if f != nil {
		return false, f
	}
	evObj := evVal.obj

	for cur := target; cur != nil; cur = cur.parent {
		curVal := cur.jsValue(vm)
		evObj.set("currentTarget", curVal)
		snapshot := append([]domListener(nil), cur.listeners...)
		for _, l := range snapshot {
			if l.typ != typ {
				continue
			}
			if _, cf := vm.callFunction(l.fn, curVal, []value{evVal}); cf != nil {
				return false, cf
			}
		}
		if !bubbles {
			break
		}
		if stop, _ := evObj.get("cancelBubble"); toBool(stop) {
			break
		}
	}
	prevented, _ := evObj.get("defaultPrevented")
	return !toBool(prevented), nil
}

// ElementByID looks up an element in the live DOM tree by its id attribute.
func (d *DOM) ElementByID(id string) *DOMElement {
	if d == nil || d.doc == nil {
		return nil
	}
	found := d.doc.findByID(id)
	if found == nil {
		return nil
	}
	return &DOMElement{node: found}
}

// LocalStorage returns a value stored in window.localStorage and whether it exists.
func (d *DOM) LocalStorage(key string) (string, bool) {
	if d == nil || d.localStore == nil {
		return "", false
	}
	v, ok := d.localStore.data[key]
	return v, ok
}

// SessionStorage returns a value stored in window.sessionStorage and whether it exists.
func (d *DOM) SessionStorage(key string) (string, bool) {
	if d == nil || d.sessionStore == nil {
		return "", false
	}
	v, ok := d.sessionStore.data[key]
	return v, ok
}

// DrainTimers executes queued setTimeout callbacks in deterministic FIFO order.
func (d *DOM) DrainTimers(ctx context.Context) error {
	if d == nil || d.vm == nil {
		return nil
	}
	d.vm.ctx = ctx
	if err := ctx.Err(); err != nil {
		return ErrInterrupted
	}
	executed := 0
	for len(d.timers) > 0 {
		if err := ctx.Err(); err != nil {
			return ErrInterrupted
		}
		t := d.timers[0]
		d.timers = d.timers[1:]
		if t.canceled {
			continue
		}
		executed++
		if executed > maxDOMTimers {
			return ErrRuntime
		}
		if _, f := d.vm.callFunction(t.fn, objVal(d.vm.globalObj), nil); f != nil {
			return d.vm.flowToError(f)
		}
	}
	return nil
}

// SetInputValue sets #id.value to val, dispatches bubbling input and change
// events on #id, and drains any resulting timers.
func (d *DOM) SetInputValue(ctx context.Context, id, val string) error {
	if d == nil || d.vm == nil || d.doc == nil {
		return ErrRuntime
	}
	d.vm.ctx = ctx
	if err := ctx.Err(); err != nil {
		return ErrInterrupted
	}
	if len(val) > maxStringBytes {
		return ErrRuntime
	}
	target := d.doc.findByID(id)
	if target == nil {
		return ErrRuntime
	}
	target.value = val
	for _, evType := range []string{"input", "change"} {
		if _, f := d.dispatchNodeEvent(d.vm, target, evType, true); f != nil {
			return d.vm.flowToError(f)
		}
	}
	return d.DrainTimers(ctx)
}

// Click dispatches a bubbling click event on #id and drains any resulting timers.
func (d *DOM) Click(ctx context.Context, id string) error {
	if d == nil || d.vm == nil || d.doc == nil {
		return ErrRuntime
	}
	d.vm.ctx = ctx
	if err := ctx.Err(); err != nil {
		return ErrInterrupted
	}
	target := d.doc.findByID(id)
	if target == nil {
		return ErrRuntime
	}
	if _, f := d.dispatchNodeEvent(d.vm, target, "click", true); f != nil {
		return d.vm.flowToError(f)
	}
	return d.DrainTimers(ctx)
}

// ResetForm resets all descendant form controls of #id to their initial HTML
// defaultValue, dispatches a bubbling reset event, and drains queued timers.
func (d *DOM) ResetForm(ctx context.Context, id string) error {
	if d == nil || d.vm == nil || d.doc == nil {
		return ErrRuntime
	}
	d.vm.ctx = ctx
	if err := ctx.Err(); err != nil {
		return ErrInterrupted
	}
	form := d.doc.findByID(id)
	if form == nil {
		return ErrRuntime
	}
	var restore func(*domNode)
	restore = func(cur *domNode) {
		if cur.kind == domElement {
			switch cur.tag {
			case "input", "select", "textarea":
				cur.value = cur.defaultValue
			}
		}
		for _, ch := range cur.children {
			restore(ch)
		}
	}
	restore(form)
	if _, f := d.dispatchNodeEvent(d.vm, form, "reset", true); f != nil {
		return d.vm.flowToError(f)
	}
	return d.DrainTimers(ctx)
}
