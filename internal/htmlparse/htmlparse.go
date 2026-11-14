// Package htmlparse parses static HTML documents into a small element tree
// using only the Go standard library.
package htmlparse

import (
	"errors"
	"html"
	"io"
	"strings"
)

// NodeType identifies the kind of an HTML tree node.
type NodeType int

const (
	DocumentNode NodeType = iota
	ElementNode
	TextNode
)

// Attr is a single HTML element attribute with a lower-case key.
type Attr struct {
	Key string
	Val string
}

// Node is one node in a parsed HTML document.
type Node struct {
	Type        NodeType
	Data        string
	Attr        []Attr
	FirstChild  *Node
	LastChild   *Node
	NextSibling *Node
}

func (n *Node) appendChild(c *Node) {
	if n.FirstChild == nil {
		n.FirstChild = c
		n.LastChild = c
		return
	}
	n.LastChild.NextSibling = c
	n.LastChild = c
}

// maxOpenElements bounds element nesting depth so pathological documents fail
// cleanly within the memory budget.
const maxOpenElements = 512

// ErrTooDeep is returned when an HTML document nests more open elements than
// maxOpenElements allows.
var ErrTooDeep = errors.New("html nesting exceeds limit")

// ErrMalformed is returned when an HTML document cannot be tokenized.
var ErrMalformed = errors.New("html could not be parsed")

var voidElements = map[string]bool{
	"area":   true,
	"base":   true,
	"br":     true,
	"col":    true,
	"embed":  true,
	"hr":     true,
	"img":    true,
	"input":  true,
	"link":   true,
	"meta":   true,
	"param":  true,
	"source": true,
	"track":  true,
	"wbr":    true,
}

var rawTextElements = map[string]bool{
	"script":   true,
	"style":    true,
	"title":    true,
	"textarea": true,
}

// blockClosesP lists elements whose opening implicitly closes an open <p>.
var blockClosesP = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"details": true, "div": true, "dl": true, "fieldset": true,
	"figcaption": true, "figure": true, "footer": true, "form": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "hgroup": true, "hr": true, "main": true, "menu": true,
	"nav": true, "ol": true, "p": true, "pre": true, "section": true,
	"table": true, "ul": true,
}

// Parse reads all bytes from r and builds an HTML node tree rooted at a
// DocumentNode.
func Parse(r io.Reader) (*Node, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	s := string(raw)
	doc := &Node{Type: DocumentNode}
	stack := []*Node{doc}

	top := func() *Node {
		return stack[len(stack)-1]
	}

	closeIfOpen := func(tag string) {
		for i := len(stack) - 1; i >= 1; i-- {
			if stack[i].Data == tag {
				stack = stack[:i]
				return
			}
		}
	}

	addText := func(txt string) {
		if txt == "" {
			return
		}
		cur := top()
		cur.appendChild(&Node{Type: TextNode, Data: html.UnescapeString(txt)})
	}

	i := 0
	n := len(s)
	for i < n {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			addText(s[i:])
			break
		}
		if lt > 0 {
			addText(s[i : i+lt])
			i += lt
		}
		// s[i] == '<'
		if i+1 >= n {
			addText("<")
			break
		}
		next := s[i+1]

		// Comment, doctype, or declaration: <! ... >
		if next == '!' {
			if strings.HasPrefix(s[i:], "<!--") {
				end := strings.Index(s[i+4:], "-->")
				if end < 0 {
					return nil, ErrMalformed
				}
				i += 4 + end + 3
				continue
			}
			end := strings.IndexByte(s[i+2:], '>')
			if end < 0 {
				return nil, ErrMalformed
			}
			i += 2 + end + 1
			continue
		}

		// Processing instruction: <? ... >
		if next == '?' {
			end := strings.IndexByte(s[i+2:], '>')
			if end < 0 {
				return nil, ErrMalformed
			}
			i += 2 + end + 1
			continue
		}

		// End tag: </tag ...>
		if next == '/' {
			j := i + 2
			if j >= n || !isASCIILetter(s[j]) {
				end := strings.IndexByte(s[j:], '>')
				if end < 0 {
					addText(s[i:])
					break
				}
				i = j + end + 1
				continue
			}
			startName := j
			for j < n && isTagNameChar(s[j]) {
				j++
			}
			tagName := strings.ToLower(s[startName:j])
			end := strings.IndexByte(s[j:], '>')
			if end < 0 {
				return nil, ErrMalformed
			}
			i = j + end + 1
			closeIfOpen(tagName)
			continue
		}

		// Start tag must begin with ASCII letter; otherwise '<' is plain text.
		if !isASCIILetter(next) {
			addText("<")
			i++
			continue
		}

		tagName, attrs, selfClosing, afterTag, ok := parseStartTag(s, i)
		if !ok {
			return nil, ErrMalformed
		}
		i = afterTag

		if blockClosesP[tagName] {
			for k := len(stack) - 1; k >= 1; k-- {
				if stack[k].Data == "p" {
					stack = stack[:k]
					break
				}
			}
		}
		if tagName == "li" && top().Data == "li" {
			stack = stack[:len(stack)-1]
		}
		if tagName == "body" {
			closeIfOpen("head")
		}

		elem := &Node{
			Type: ElementNode,
			Data: tagName,
			Attr: attrs,
		}
		top().appendChild(elem)

		if selfClosing || voidElements[tagName] {
			continue
		}

		if rawTextElements[tagName] {
			rawContent, nextIdx, found := consumeRawText(s, i, tagName)
			if !found {
				return nil, ErrMalformed
			}
			if rawContent != "" {
				txt := rawContent
				if tagName == "title" || tagName == "textarea" {
					txt = html.UnescapeString(txt)
				}
				elem.appendChild(&Node{Type: TextNode, Data: txt})
			}
			i = nextIdx
			continue
		}

		stack = append(stack, elem)
		if len(stack) > maxOpenElements {
			return nil, ErrTooDeep
		}
	}

	coalesceText(doc)
	return doc, nil
}

func coalesceText(n *Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == TextNode && c.NextSibling != nil && c.NextSibling.Type == TextNode {
			var b strings.Builder
			b.WriteString(c.Data)
			cur := c.NextSibling
			for cur != nil && cur.Type == TextNode {
				b.WriteString(cur.Data)
				cur = cur.NextSibling
			}
			c.Data = b.String()
			c.NextSibling = cur
			if cur == nil {
				n.LastChild = c
			}
		}
		if c.FirstChild != nil {
			coalesceText(c)
		}
	}
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isTagNameChar(b byte) bool {
	return isASCIILetter(b) || (b >= '0' && b <= '9') || b == '-' || b == ':'
}

func isAttrNameStop(b byte) bool {
	return isHTMLSpace(b) || b == '=' || b == '>' || b == '/'
}

func isHTMLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func parseStartTag(s string, start int) (tag string, attrs []Attr, selfClosing bool, next int, ok bool) {
	n := len(s)
	j := start + 1
	for j < n && isTagNameChar(s[j]) {
		j++
	}
	tag = strings.ToLower(s[start+1 : j])
	seen := map[string]bool{}

	for {
		for j < n && isHTMLSpace(s[j]) {
			j++
		}
		if j >= n {
			return "", nil, false, 0, false
		}
		if s[j] == '>' {
			return tag, attrs, false, j + 1, true
		}
		if s[j] == '/' {
			j++
			if j < n && s[j] == '>' {
				return tag, attrs, true, j + 1, true
			}
			continue
		}

		attrStart := j
		for j < n && !isAttrNameStop(s[j]) {
			j++
		}
		if j == attrStart {
			return "", nil, false, 0, false
		}
		key := strings.ToLower(s[attrStart:j])

		for j < n && isHTMLSpace(s[j]) {
			j++
		}
		val := ""
		if j < n && s[j] == '=' {
			j++
			for j < n && isHTMLSpace(s[j]) {
				j++
			}
			if j >= n {
				return "", nil, false, 0, false
			}
			switch s[j] {
			case '"', '\'':
				quote := s[j]
				j++
				valStart := j
				endQuote := strings.IndexByte(s[j:], quote)
				if endQuote < 0 {
					return "", nil, false, 0, false
				}
				val = html.UnescapeString(s[valStart : j+endQuote])
				j += endQuote + 1
			default:
				valStart := j
				for j < n && !isHTMLSpace(s[j]) && s[j] != '>' {
					j++
				}
				val = html.UnescapeString(s[valStart:j])
			}
		}

		// HTML5 keeps only the first occurrence when an attribute is repeated.
		if !seen[key] {
			seen[key] = true
			attrs = append(attrs, Attr{Key: key, Val: val})
		}
	}
}

func consumeRawText(s string, start int, tag string) (content string, next int, ok bool) {
	n := len(s)
	j := start
	needle := "</" + tag
	for j < n {
		idx := strings.IndexByte(s[j:], '<')
		if idx < 0 {
			return "", 0, false
		}
		pos := j + idx
		if pos+len(needle) < n && strings.EqualFold(s[pos:pos+len(needle)], needle) {
			after := s[pos+len(needle)]
			if isHTMLSpace(after) || after == '>' || after == '/' {
				end := strings.IndexByte(s[pos+len(needle):], '>')
				if end < 0 {
					return "", 0, false
				}
				return s[start:pos], pos + len(needle) + end + 1, true
			}
		}
		j = pos + 1
	}
	return "", 0, false
}
