package channel

import (
	"bufio"
	"bytes"
	"fmt"
	stdhtml "html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var plainTextMarkdownURLRegexp = regexp.MustCompile(`^(?:http|https|ftp)://[^\s<>"']+`)

type plainTextURLParser struct{ linkify parser.InlineParser }

// Linkify misses schemes after CJK text because it only triggers on ASCII punctuation or whitespace.
func (plainTextURLParser) Trigger() []byte { return []byte{':'} }

func (p plainTextURLParser) Parse(parent gast.Node, block text.Reader, pc parser.Context) gast.Node {
	line, colonPosition := block.Position()
	colon := colonPosition.Start
	if colon < 0 || colon >= len(block.Source()) || block.Source()[colon] != ':' {
		return nil
	}

	for _, scheme := range []string{"https", "http", "ftp"} {
		start := colon - len(scheme)
		if start < 0 || !bytes.Equal(block.Source()[start:colon], []byte(scheme)) {
			continue
		}
		last, ok := parent.LastChild().(*gast.Text)
		if !ok || last.Segment.Start > start || last.Segment.Stop != colon || !bytes.HasSuffix(last.Value(block.Source()), []byte(scheme)) {
			return nil
		}

		block.SetPosition(line, colonPosition.WithStart(start))
		node := p.linkify.Parse(parent, block, pc)
		if node == nil {
			block.SetPosition(line, colonPosition)
			return nil
		}
		node.SetPos(start)

		if last.Segment.Start == start && last.Segment.Padding == 0 {
			parent.RemoveChild(parent, last)
		} else {
			last.Segment = last.Segment.WithStop(start)
		}
		return node
	}
	return nil
}

var plainTextMarkdownParser = goldmark.New(
	goldmark.WithExtensions(extension.Table),
	goldmark.WithParserOptions(parser.WithInlineParsers(util.Prioritized(
		plainTextURLParser{extension.NewLinkifyParser(extension.WithLinkifyURLRegexp(plainTextMarkdownURLRegexp))}, 999,
	))),
)

// PlainTextMarkdown renders Markdown formatting as readable plain text.
func PlainTextMarkdown(markdown string, literals ...string) string {
	markdown, restore := protectMarkdownLiterals(markdown, literals)
	source := []byte(markdown)
	document := plainTextMarkdownParser.Parser().Parse(text.NewReader(source))
	return restore(renderMarkdownBlocks(document, source, 0))
}

// MarkdownCodeLiterals returns the literal contents of code spans and blocks.
func MarkdownCodeLiterals(markdown string) []string {
	source := []byte(markdown)
	document := plainTextMarkdownParser.Parser().Parse(text.NewReader(source))
	var literals []string
	seen := make(map[string]bool)
	_ = gast.Walk(document, func(node gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		var literal string
		switch node.(type) {
		case *gast.CodeSpan:
			literal = renderMarkdownInline(node, source)
		case *gast.CodeBlock, *gast.FencedCodeBlock:
			literal = string(node.Lines().Value(source))
			if strings.HasSuffix(literal, "\r\n") {
				literal = strings.TrimSuffix(literal, "\r\n")
			} else {
				literal = strings.TrimSuffix(literal, "\n")
			}
		default:
			return gast.WalkContinue, nil
		}
		if literal != "" && !seen[literal] {
			seen[literal] = true
			literals = append(literals, literal)
		}
		return gast.WalkSkipChildren, nil
	})
	return literals
}

func protectMarkdownLiterals(markdown string, literals []string) (string, func(string) string) {
	literals = append([]string(nil), literals...)
	unique := literals[:0]
	seen := make(map[string]bool)
	for _, literal := range literals {
		if literal != "" && !seen[literal] {
			seen[literal] = true
			unique = append(unique, literal)
		}
	}
	if len(unique) == 0 {
		return markdown, func(rendered string) string { return rendered }
	}
	literals = unique
	sort.SliceStable(literals, func(i, j int) bool { return len(literals[i]) > len(literals[j]) })

	var replacePairs, restorePairs []string
	decoded := stdhtml.UnescapeString(markdown)
	prefix := string(unusedPrivateUseRune(markdown, decoded))
	sequence := 0
	for _, literal := range literals {
		placeholder := prefix + strconv.Itoa(sequence) + "\ue001"
		sequence++
		replacePairs = append(replacePairs, literal, placeholder)
		restorePairs = append(restorePairs, placeholder, literal)
	}
	return strings.NewReplacer(replacePairs...).Replace(markdown), func(rendered string) string {
		return strings.NewReplacer(restorePairs...).Replace(rendered)
	}
}

func unusedPrivateUseRune(markdown, decoded string) rune {
	for _, bounds := range [][2]rune{{0xE000, 0xF8FF}, {0xF0000, 0xFFFFD}, {0x100000, 0x10FFFD}} {
		for candidate := bounds[0]; candidate <= bounds[1]; candidate++ {
			if !strings.ContainsRune(markdown, candidate) && !strings.ContainsRune(decoded, candidate) {
				return candidate
			}
		}
	}
	panic("no unused private-use character for Markdown literal placeholders")
}

func renderMarkdownBlocks(parent gast.Node, source []byte, depth int) string {
	blocks := make([]string, 0, parent.ChildCount())
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		if block := renderMarkdownBlock(child, source, depth); block != "" {
			blocks = append(blocks, block)
		}
	}
	return strings.Join(blocks, "\n\n")
}

func renderMarkdownBlock(node gast.Node, source []byte, depth int) string {
	switch n := node.(type) {
	case *gast.Paragraph, *gast.Heading:
		return renderMarkdownInline(node, source)
	case *gast.Document, *gast.Blockquote:
		return renderMarkdownBlocks(node, source, depth)
	case *gast.List:
		return renderMarkdownList(n, source, depth)
	case *gast.CodeBlock, *gast.FencedCodeBlock:
		return string(node.Lines().Value(source))
	case *gast.HTMLBlock:
		return string(n.Text(source))
	case *gast.ThematicBreak:
		return ""
	case *extast.Table:
		rows := make([]string, 0, node.ChildCount())
		for row := node.FirstChild(); row != nil; row = row.NextSibling() {
			if rendered := renderMarkdownBlock(row, source, depth); rendered != "" {
				rows = append(rows, rendered)
			}
		}
		return strings.Join(rows, "\n")
	case *extast.TableHeader, *extast.TableRow:
		cells := make([]string, 0, node.ChildCount())
		for cell := node.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, renderMarkdownInline(cell, source))
		}
		return strings.Join(cells, " | ")
	case *extast.TableCell:
		return renderMarkdownInline(node, source)
	case *gast.ListItem:
		return renderMarkdownBlocks(node, source, depth)
	default:
		return renderMarkdownInline(node, source)
	}
}

func renderMarkdownList(list *gast.List, source []byte, depth int) string {
	items := make([]string, 0, list.ChildCount())
	indent := strings.Repeat("  ", depth)
	for item, index := list.FirstChild(), 0; item != nil; item, index = item.NextSibling(), index+1 {
		marker := "• "
		if list.IsOrdered() {
			marker = fmt.Sprintf("%d) ", list.Start+index)
		}
		var body strings.Builder
		first := true
		for child := item.FirstChild(); child != nil; child = child.NextSibling() {
			block := renderMarkdownBlock(child, source, depth+1)
			if block == "" {
				continue
			}
			if first {
				body.WriteString(indent + marker)
				body.WriteString(block)
				first = false
			} else if _, nested := child.(*gast.List); nested {
				body.WriteByte('\n')
				body.WriteString(block)
			} else {
				body.WriteByte('\n')
				body.WriteString(indent + strings.Repeat(" ", utf8.RuneCountInString(marker)))
				body.WriteString(block)
			}
		}
		if first {
			body.WriteString(indent + marker)
		}
		items = append(items, body.String())
	}
	return strings.Join(items, "\n")
}

func renderMarkdownInline(parent gast.Node, source []byte) string {
	var out strings.Builder
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		switch n := child.(type) {
		case *gast.Text:
			if n.IsRaw() {
				out.Write(n.Value(source))
			} else {
				out.WriteString(unescapeMarkdownText(n.Value(source)))
			}
			if n.SoftLineBreak() || n.HardLineBreak() {
				out.WriteByte('\n')
			}
		case *gast.String:
			out.Write(n.Value)
		case *gast.CodeSpan:
			for part := n.FirstChild(); part != nil; part = part.NextSibling() {
				out.Write(part.(*gast.Text).Value(source))
			}
		case *gast.Link:
			label := renderMarkdownInline(n, source)
			writeMarkdownLink(&out, label, unescapeMarkdownText(n.Destination))
		case *gast.Image:
			label := renderMarkdownInline(n, source)
			writeMarkdownLink(&out, label, unescapeMarkdownText(n.Destination))
		case *gast.AutoLink:
			out.WriteString(unescapeMarkdownText(n.Label(source)))
		case *gast.RawHTML:
			out.Write(n.Segments.Value(source))
		default:
			out.WriteString(renderMarkdownInline(child, source))
		}
	}
	return out.String()
}

func writeMarkdownLink(out *strings.Builder, label, destination string) {
	if label == "" {
		out.WriteString(destination)
		return
	}
	out.WriteString(label)
	if destination != "" && label != destination {
		out.WriteString(" (")
		out.WriteString(destination)
		out.WriteByte(')')
	}
}

func unescapeMarkdownText(source []byte) string {
	var rendered bytes.Buffer
	writer := bufio.NewWriter(&rendered)
	gmhtml.NewWriter().Write(writer, source)
	_ = writer.Flush()
	return stdhtml.UnescapeString(rendered.String())
}
