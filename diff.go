package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// diffPalette holds the shades used for one kind of change. Tables keep their
// header / odd / even structure, each in a shade of the same colour.
type diffPalette struct {
	class, block, head, odd, even, border string
}

var diffPalettes = []diffPalette{
	{"diff-add", "#e6ffec", "#a7e3b8", "#effff3", "#d2f5dc", "#b7e4c7"},
	{"diff-del", "#ffebe9", "#f5b5b0", "#fff1ef", "#fcd9d6", "#f5c2bf"},
	{"diff-old", "#eaeef2", "#c8cfd6", "#f3f5f7", "#e1e5ea", "#d0d7de"},
	{"diff-mod", "#fff8c5", "#f7d95a", "#fffbe0", "#fff0a0", "#ecd98a"},
}

// diffCSS colours blocks produced by buildDiffMarkdown. Only colours change;
// they are forced so they survive printing to PDF.
func diffCSS() string {
	var b strings.Builder
	b.WriteString("<style>\n")
	for _, p := range diffPalettes {
		c := "." + p.class
		fmt.Fprintf(&b, `%[1]s { background: %[2]s; -webkit-print-color-adjust: exact; print-color-adjust: exact; }
%[1]s table { background: %[6]s !important; border-color: %[7]s !important; }
%[1]s th, %[1]s td { border-color: %[7]s !important; }
%[1]s th, %[1]s thead tr, %[1]s tr.table-cont-header td, %[1]s tr.table-cont-header th { background: %[3]s !important; color: #222 !important; }
%[1]s tbody tr, %[1]s tbody td { background: %[4]s !important; }
%[1]s tbody tr:nth-child(odd), %[1]s tbody tr.tr-odd, %[1]s tbody tr:nth-child(odd) td, %[1]s tbody tr.tr-odd td { background: %[4]s !important; }
%[1]s tbody tr:nth-child(even), %[1]s tbody tr.tr-even, %[1]s tbody tr:nth-child(even) td, %[1]s tbody tr.tr-even td { background: %[5]s !important; }
`, c, p.block, p.head, p.odd, p.even, p.odd, p.border)
	}
	b.WriteString("</style>")
	return b.String()
}

// enableDiffRendering lets the Markdown renderer pass through the <div>
// wrappers that buildDiffMarkdown emits.
func enableDiffRendering() {
	markdownParser = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)
}

// splitBlocks splits Markdown into blocks separated by blank lines. Blank lines
// inside fenced code blocks do not split.
func splitBlocks(src string) []string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	var blocks []string
	var cur []string
	fence := ""

	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.Join(cur, "\n"))
			cur = nil
		}
	}

	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			cur = append(cur, line)
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:3]
			cur = append(cur, line)
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return blocks
}

func normBlock(b string) string {
	return strings.Join(strings.Fields(b), " ")
}

type diffOp struct {
	kind string // "eq", "add", "del", "mod"
	text string
	old  string // previous version of a "mod" block
}

// diffBlocks compares old and new block lists using an LCS, then pairs similar
// removed/added blocks in the same hunk as modifications.
func diffBlocks(oldB, newB []string) []diffOp {
	n, m := len(oldB), len(newB)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if normBlock(oldB[i]) == normBlock(newB[j]) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var ops []diffOp
	var dels, adds []string
	flush := func() {
		ops = append(ops, pairHunk(dels, adds)...)
		dels, adds = nil, nil
	}

	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && normBlock(oldB[i]) == normBlock(newB[j]):
			flush()
			ops = append(ops, diffOp{kind: "eq", text: newB[j]})
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			adds = append(adds, newB[j])
			j++
		default:
			dels = append(dels, oldB[i])
			i++
		}
	}
	flush()
	return ops
}

// pairHunk turns a run of removed and added blocks into operations, treating
// similar blocks (in order) as modified.
func pairHunk(dels, adds []string) []diffOp {
	var ops []diffOp
	di := 0
	for _, a := range adds {
		matched := -1
		for k := di; k < len(dels); k++ {
			if similarity(dels[k], a) >= 0.3 {
				matched = k
				break
			}
		}
		if matched < 0 {
			ops = append(ops, diffOp{kind: "add", text: a})
			continue
		}
		for ; di < matched; di++ {
			ops = append(ops, diffOp{kind: "del", text: dels[di]})
		}
		di = matched + 1
		ops = append(ops, diffOp{"mod", a, dels[matched]})
	}
	for ; di < len(dels); di++ {
		ops = append(ops, diffOp{kind: "del", text: dels[di]})
	}
	return ops
}

// similarity is the word-level Jaccard similarity of two blocks.
func similarity(a, b string) float64 {
	wa := map[string]bool{}
	for _, w := range strings.Fields(a) {
		wa[w] = true
	}
	wb := map[string]bool{}
	for _, w := range strings.Fields(b) {
		wb[w] = true
	}
	if len(wa) == 0 && len(wb) == 0 {
		return 1
	}
	inter := 0
	for w := range wa {
		if wb[w] {
			inter++
		}
	}
	union := len(wa) + len(wb) - inter
	return float64(inter) / float64(union)
}

var headingMarker = regexp.MustCompile(`(?m)^ {0,3}#{1,6}!?[ \t]+(.*?)[ \t]*#*[ \t]*$`)

// plainDeleted keeps removed headings and markers out of the table of contents
// and chapter handling by turning them into bold text.
func plainDeleted(block string) string {
	if strings.TrimSpace(block) == tocMarker {
		return "*" + tocMarker + "*"
	}
	return headingMarker.ReplaceAllString(block, "**$1**")
}

// buildDiffMarkdown merges oldSrc and newSrc into one Markdown document where
// added blocks are green, removed blocks red and modified blocks yellow.
func buildDiffMarkdown(oldSrc, newSrc string) (string, bool) {
	ops := diffBlocks(splitBlocks(oldSrc), splitBlocks(newSrc))

	var out strings.Builder
	changed := false
	wrap := func(class, body string) {
		out.WriteString("<div class=\"" + class + "\">\n\n" + body + "\n\n</div>\n\n")
	}

	for _, op := range ops {
		switch op.kind {
		case "eq":
			out.WriteString(op.text + "\n\n")
		case "add":
			changed = true
			wrap("diff-add", op.text)
		case "mod":
			changed = true
			wrap("diff-old", plainDeleted(op.old))
			wrap("diff-mod", op.text)
		case "del":
			changed = true
			wrap("diff-del", plainDeleted(op.text))
		}
	}

	if !changed {
		return newSrc, false
	}
	return diffCSS() + "\n\n" + out.String(), true
}
