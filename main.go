// mdexport converts Markdown documents to PDF using Chromium.
// It supports configurable page margins and a repeated image footer.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

const (
	installedFooterPath = "/usr/src/footer"
	legacyFooterPath    = "/usr/src/pdfexporter/footer.png"
	localFooterPath     = "public/footer.png"

	defaultTimeout = 2 * time.Minute

	// tocMarker is the Markdown line replaced by an auto-generated List of Content.
	tocMarker = "!list-of-content"
	// tocMaxLevel is the deepest heading level (1-4) included in the List of Content.
	tocMaxLevel = 4

	// docTitleClass marks the main title written as "#! Title".
	docTitleClass = "doc-title"

	// tocPlaceholder stands in for the List of Content until page numbers are known.
	tocPlaceholder = "<!--list-of-content-->"
)

type Config struct {
	Input        string
	Output       string
	Footer       string
	ChangeFooter string

	MarginTop    float64
	MarginRight  float64
	MarginBottom float64
	MarginLeft   float64

	Timeout time.Duration
}

type PageMargins struct {
	Top    float64
	Right  float64
	Bottom float64
	Left   float64
}

var markdownParser = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// convertedDoc is the result of converting Markdown to HTML.
type convertedDoc struct {
	HTML       string     // contains tocPlaceholder where the List of Content goes
	TOC        []tocEntry // headings listed in the List of Content
	HasTOC     bool       // the document has a "!list-of-content" marker
	ChapterIDs []string   // ids of "#" headings; each one starts a new page
	HasTitle   bool       // the document has a "#!" main title
}

// pageNumbering controls the page number printed at the top right.
type pageNumbering struct {
	Hidden map[int]bool // 1-based pages that show no number
	Total  int          // total page count when known, 0 otherwise
	Probe  []string     // heading ids linked invisibly so Chromium records their page
}

type tocEntry struct {
	Level int
	ID    string
	Title string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// run executes the application.
func run() error {
	config, err := parseFlags()
	if err != nil {
		return err
	}

	if config.ChangeFooter != "" {
		if err := replaceInstalledFooter(config.ChangeFooter); err != nil {
			return fmt.Errorf("replace footer: %w", err)
		}

		fmt.Printf("Installed footer replaced with %s\n", config.ChangeFooter)
		return nil
	}

	if config.Input == "" {
		return errors.New("input Markdown file is required; use mdexport file.md")
	}

	return exportMarkdown(config)
}

// parseFlags parses command-line arguments.
func parseFlags() (Config, error) {
	var config Config

	fs := flag.NewFlagSet("mdexport", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&config.Input, "in", "", "input Markdown file (required)")
	fs.StringVar(&config.Output, "out", "", "output PDF file")
	fs.StringVar(&config.Footer, "footer", "", "footer image path")
	fs.StringVar(&config.ChangeFooter, "changefooter", "", "replace installed footer image")

	fs.Float64Var(&config.MarginTop, "margin-top", 15, "top margin in mm")
	fs.Float64Var(&config.MarginRight, "margin-right", 15, "right content margin in mm")
	fs.Float64Var(&config.MarginBottom, "margin-bottom", 25, "bottom margin in mm")
	fs.Float64Var(&config.MarginLeft, "margin-left", 15, "left content margin in mm")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return config, err
	}

	positional := fs.Args()
	if config.Input != "" && len(positional) > 0 {
		return config, errors.New("provide the input file either as a positional argument or with -in, not both")
	}
	if config.Input == "" {
		if len(positional) > 1 {
			return config, errors.New("only one input Markdown file may be provided")
		}
		if len(positional) == 1 {
			config.Input = positional[0]
		}
	}

	config.Timeout = defaultTimeout

	return config, nil
}

// exportMarkdown executes the complete Markdown-to-PDF workflow.
func exportMarkdown(config Config) error {
	input, err := filepath.Abs(config.Input)
	if err != nil {
		return fmt.Errorf("resolve input path: %w", err)
	}

	if err := validateFile(input); err != nil {
		return fmt.Errorf("input: %w", err)
	}

	output := config.Output
	if output == "" {
		output = strings.TrimSuffix(input, filepath.Ext(input)) + ".pdf"
	}

	output, err = filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}

	if samePath(input, output) {
		return errors.New("input and output paths must be different")
	}

	footer, err := resolveFooterPath(config.Footer)
	if err != nil {
		return fmt.Errorf("resolve footer: %w", err)
	}

	if samePath(output, footer) {
		return errors.New("output path must not overwrite the footer image")
	}

	if err := validateFile(footer); err != nil {
		return fmt.Errorf("footer: %w", err)
	}

	margins := PageMargins{
		Top:    config.MarginTop,
		Right:  config.MarginRight,
		Bottom: config.MarginBottom,
		Left:   config.MarginLeft,
	}

	if err := validateMargins(margins); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	tmp, err := os.MkdirTemp("", "mdexport-*")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	markdown, err := os.ReadFile(input)
	if err != nil {
		return fmt.Errorf("read Markdown: %w", err)
	}

	doc, err := convertMarkdown(markdown)
	if err != nil {
		return fmt.Errorf("convert Markdown: %w", err)
	}

	browser, err := findChromium()
	if err != nil {
		return err
	}

	assetBase := filepath.Dir(input)
	numbering := pageNumbering{Hidden: map[int]bool{}}
	if doc.HasTitle {
		numbering.Hidden[1] = true
	}

	// Pass 1: find the page of every heading, to fill in the List of Content
	// and to hide the page number on the first page of each "#" chapter.
	var headingPages map[string]int
	if probe := doc.probeIDs(); len(probe) > 0 {
		total, pages, err := probeHeadingPages(browser, doc, probe, footer, assetBase, margins, tmp, config.Timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot measure heading pages: %v\n", err)
		} else {
			headingPages = pages
			numbering.Total = total
			for _, id := range doc.ChapterIDs {
				if page, ok := pages[id]; ok {
					numbering.Hidden[page] = true
				}
			}
		}
	}

	document := buildHTMLDocument(doc.html(headingPages), footer, assetBase, margins, numbering)

	htmlPath := filepath.Join(tmp, "document.html")
	if err := os.WriteFile(htmlPath, []byte(document), 0600); err != nil {
		return fmt.Errorf("write HTML document: %w", err)
	}

	if err := exportPDF(browser, htmlPath, output, tmp, config.Timeout); err != nil {
		return fmt.Errorf("export PDF: %w", err)
	}

	if err := validatePDF(output); err != nil {
		return fmt.Errorf("validate PDF: %w", err)
	}

	fmt.Printf("PDF saved to %s\n", output)
	return nil
}

// convertMarkdown converts Markdown content into HTML. A paragraph starting
// with "#! " becomes the main title, and a paragraph consisting only of
// "!list-of-content" is replaced by a generated List of Content.
func convertMarkdown(source []byte) (convertedDoc, error) {
	doc := markdownParser.Parser().Parse(text.NewReader(source))
	result := convertedDoc{HasTitle: convertTitles(doc, source)}

	var entries []tocEntry
	hasMarker := false

	err := ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		switch n := node.(type) {
		case *ast.Heading:
			if isDocTitle(n) {
				return ast.WalkSkipChildren, nil
			}

			id := headingID(n)
			if n.Level == 1 && id != "" {
				result.ChapterIDs = append(result.ChapterIDs, id)
			}
			if n.Level <= tocMaxLevel {
				entries = append(entries, tocEntry{
					Level: n.Level,
					ID:    id,
					Title: strings.TrimSpace(nodeText(n, source)),
				})
			}
			return ast.WalkSkipChildren, nil
		case *ast.Paragraph:
			if strings.TrimSpace(nodeText(n, source)) == tocMarker {
				hasMarker = true
			}
			return ast.WalkSkipChildren, nil
		}

		return ast.WalkContinue, nil
	})
	if err != nil {
		return result, err
	}

	var output bytes.Buffer
	if err := markdownParser.Renderer().Render(&output, source, doc); err != nil {
		return result, err
	}

	result.HTML = output.String()
	if hasMarker {
		result.HTML = strings.ReplaceAll(result.HTML, "<p>"+tocMarker+"</p>", tocPlaceholder)
		result.TOC = entries
		result.HasTOC = true
	}

	return result, nil
}

// html returns the document body with the List of Content filled in. pages maps
// heading ids to page numbers; nil renders placeholder numbers of equal width.
func (d convertedDoc) html(pages map[string]int) string {
	out := d.HTML
	if d.HasTOC {
		out = strings.ReplaceAll(out, tocPlaceholder, buildTOC(d.TOC, pages))
	}
	return transformTables(out, pages)
}

// probeIDs returns the heading ids whose page must be measured.
func (d convertedDoc) probeIDs() []string {
	seen := map[string]bool{}
	var ids []string

	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	for _, id := range d.ChapterIDs {
		add(id)
	}
	for _, entry := range d.TOC {
		add(entry.ID)
	}
	for table, rows := range tableRowCounts(d.HTML) {
		for row := 0; row < rows; row++ {
			add(tableRowID(table, row))
		}
	}

	return ids
}

// convertTitles turns top-level paragraphs starting with "#! " into level 1
// headings of class doc-title. It reports whether any title was found.
func convertTitles(doc ast.Node, source []byte) bool {
	found := false

	for node := doc.FirstChild(); node != nil; node = node.NextSibling() {
		para, ok := node.(*ast.Paragraph)
		if !ok {
			continue
		}

		first, ok := para.FirstChild().(*ast.Text)
		if !ok {
			continue
		}

		value := first.Segment.Value(source)
		if len(value) < 3 || !bytes.HasPrefix(value, []byte("#!")) || (value[2] != ' ' && value[2] != '\t') {
			continue
		}

		start := first.Segment.Start + 2
		for start < first.Segment.Stop && (source[start] == ' ' || source[start] == '\t') {
			start++
		}
		first.Segment = first.Segment.WithStart(start)

		heading := ast.NewHeading(1)
		heading.SetAttributeString("class", []byte(docTitleClass))
		for child := para.FirstChild(); child != nil; {
			next := child.NextSibling()
			heading.AppendChild(heading, child)
			child = next
		}

		doc.ReplaceChild(doc, para, heading)
		node = heading
		found = true
	}

	return found
}

// isDocTitle reports whether a heading is the "#!" main title.
func isDocTitle(heading *ast.Heading) bool {
	value, ok := heading.AttributeString("class")
	if !ok {
		return false
	}
	class, _ := value.([]byte)
	return string(class) == docTitleClass
}

// headingID returns the id attribute of a heading, or "".
func headingID(heading *ast.Heading) string {
	value, ok := heading.AttributeString("id")
	if !ok {
		return ""
	}
	id, _ := value.([]byte)
	return string(id)
}

// nodeText returns the plain text content of an inline-containing node.
func nodeText(node ast.Node, source []byte) string {
	var buf strings.Builder

	_ = ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		switch t := n.(type) {
		case *ast.Text:
			buf.Write(t.Segment.Value(source))
			if t.SoftLineBreak() || t.HardLineBreak() {
				buf.WriteByte(' ')
			}
		case *ast.String:
			buf.Write(t.Value)
		case *ast.RawHTML:
			return ast.WalkSkipChildren, nil
		}

		return ast.WalkContinue, nil
	})

	return buf.String()
}

// buildTOC renders the List of Content as nested lists linking to headings.
func buildTOC(entries []tocEntry, pages map[string]int) string {
	var buf strings.Builder

	buf.WriteString(`<nav class="toc">` + "\n")
	buf.WriteString(`<p class="toc-title">List of Content</p>` + "\n")

	if len(entries) == 0 {
		buf.WriteString("</nav>\n")
		return buf.String()
	}

	base := entries[0].Level
	for _, entry := range entries {
		if entry.Level < base {
			base = entry.Level
		}
	}

	openItem := func(level int) {
		buf.WriteString(`<li class="toc-l` + strconv.Itoa(level) + `">`)
	}

	depth := 0
	for _, entry := range entries {
		target := entry.Level - base + 1

		if depth == 0 {
			buf.WriteString("<ul>\n")
			depth = 1
			openItem(depth)
		} else if target > depth {
			for depth < target {
				depth++
				buf.WriteString("\n<ul>\n")
				openItem(depth)
			}
		} else {
			for depth > target {
				buf.WriteString("</li>\n</ul>\n")
				depth--
			}
			buf.WriteString("</li>\n")
			openItem(depth)
		}

		title := `<span class="toc-text">` + htmlEscape(entry.Title) + `</span>`
		if entry.ID == "" {
			buf.WriteString(`<span class="toc-entry">` + title + `</span>`)
			continue
		}

		page := "000"
		if pages != nil {
			if number, ok := pages[entry.ID]; ok {
				page = strconv.Itoa(number)
			} else {
				page = ""
			}
		}

		buf.WriteString(`<a class="toc-entry" href="#` + htmlEscape(entry.ID) + `">` + title +
			`<span class="toc-dots"></span><span class="toc-page">` + page + `</span></a>`)
	}

	for depth > 0 {
		buf.WriteString("</li>\n</ul>\n")
		depth--
	}

	buf.WriteString("</nav>\n")
	return buf.String()
}

// htmlEscape escapes text for safe inclusion in HTML content and attributes.
func htmlEscape(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	).Replace(value)
}

// buildHTMLDocument generates the complete HTML document.
func buildHTMLDocument(body, footerPath, assetBasePath string, margins PageMargins, numbering pageNumbering) string {
	footerURL := filepath.ToSlash(footerPath)
	assetBaseURL := (&url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(assetBasePath) + "/",
	}).String()

	// Convert the local file path into a file:// URL.
	footerURI := "file://" + footerURL
	if !strings.HasPrefix(footerURI, "file:///") {
		footerURI = "file:///" + strings.TrimPrefix(footerURI, "/")
	}

	footerURI = strings.ReplaceAll(footerURI, " ", "%20")
	footerURI = strings.ReplaceAll(footerURI, "#", "%23")

	style := fmt.Sprintf(`
@page {
	size: A4;
	margin: %smm 0 %smm 0;

	@bottom-center {
		content: "";
		width: 100%%;
		vertical-align: bottom;
		transform: translateY(6mm);
		margin: 0;
		padding: 0;
		background-image: url("%s");
		background-repeat: no-repeat;
		background-position: center bottom;
		background-size: 100%% auto;
	}
}

%s

* {
	box-sizing: border-box;
}

html {
	font-family: Aptos, Arial, sans-serif;
	font-size: 11pt;
	color: #222;
}

body {
	margin: 0;
	padding: 0 %smm 0 %smm;
	line-height: 1.5;
	word-wrap: break-word;
}

h1, h2, h3, h4, h5, h6 {
	page-break-after: avoid;
	break-after: avoid;
}

/* Every chapter ("#") starts on a new page. */
h1 {
	break-before: page;
	page-break-before: always;
}

/* Main title ("#!"). */
h1.doc-title {
	break-before: auto;
	page-break-before: auto;
	font-size: 2.4em;
	padding-bottom: 8px;
	margin-bottom: 28px;
	border-bottom: 2px solid #222;
}

p {
	margin: 0 0 10px;
}

img {
	max-width: 100%%;
	height: auto;
}

pre {
	white-space: pre-wrap;
	overflow-wrap: anywhere;
	background: #f4f4f4;
	padding: 10px;
	border-radius: 4px;
}

code {
	font-family: monospace;
	overflow-wrap: anywhere;
}

table {
	border-collapse: collapse;
	width: 100%%;
	margin: 4px 0 14px;
	font-size: 0.95em;
	border: 1px solid #b8c4d6;
}

th, td {
	border: 1px solid #c9d3e0;
	padding: 6px 10px;
	text-align: left;
	vertical-align: top;
}

thead {
	display: table-header-group;
}

th {
	background: #1f4e8c;
	color: #fff;
	font-weight: bold;
	border-color: #1f4e8c;
	-webkit-print-color-adjust: exact;
	print-color-adjust: exact;
}

tbody tr:nth-child(odd) {
	background: #ffffff;
}

tbody tr:nth-child(even) {
	background: #e9f0f9;
	-webkit-print-color-adjust: exact;
	print-color-adjust: exact;
}

tbody tr.tr-odd {
	background: #ffffff;
}

tbody tr.tr-even {
	background: #e9f0f9;
	-webkit-print-color-adjust: exact;
	print-color-adjust: exact;
}

tr {
	break-inside: avoid;
}

/* Header repeated on a following page: duller than the main header. */
tbody tr.table-cont-header,
tbody tr.table-cont-header th {
	background: #5f6f86;
	border-color: #5f6f86;
	break-after: avoid;
	-webkit-print-color-adjust: exact;
	print-color-adjust: exact;
}

blockquote {
	margin: 10px 0;
	padding-left: 12px;
	border-left: 3px solid #ccc;
	color: #555;
}

a {
	color: #0563c1;
	text-decoration: underline;
}

hr {
	border: 0;
	border-top: 1px solid #ccc;
	margin: 16px 0;
}

ul, ol {
	padding-left: 24px;
}

li {
	margin-bottom: 4px;
}

.toc {
	margin: 0 0 16px;
	break-after: page;
	page-break-after: always;
}

.toc-title {
	font-size: 1.5em;
	font-weight: bold;
	letter-spacing: 0.5px;
	text-transform: uppercase;
	color: #111;
	margin: 0 0 12px;
	padding-bottom: 6px;
	border-bottom: 2px solid #222;
}

.toc ul {
	list-style: none;
	padding-left: 0;
	margin: 0;
}

.toc ul ul {
	margin: 2px 0 4px 6px;
	padding-left: 16px;
	border-left: 2px solid #d5dbe5;
}

.toc li {
	margin: 0;
	break-inside: avoid;
}

.toc a {
	color: inherit;
	text-decoration: none;
}

.toc-entry {
	display: flex;
	align-items: baseline;
	padding: 2px 0;
}

.toc-text {
	flex: 0 1 auto;
	min-width: 0;
}

.toc-dots {
	flex: 1 1 24px;
	min-width: 24px;
	margin: 0 6px;
	border-bottom: 1px dotted #999;
}

.toc-page {
	flex: 0 0 auto;
	min-width: 2em;
	text-align: right;
	font-variant-numeric: tabular-nums;
}

/* Level 1: bold, separated by a rule. */
.toc-l1 {
	margin-top: 8px;
}

.toc-l1:first-child {
	margin-top: 0;
}

.toc-l1 > .toc-entry {
	font-size: 1.08em;
	font-weight: bold;
	color: #111;
	padding: 4px 0;
	border-bottom: 1px solid #e3e3e3;
}

/* Level 2: semibold with a triangle marker. */
.toc-l2 > .toc-entry {
	font-weight: 600;
	color: #2a2a2a;
}

.toc-l2 > .toc-entry::before {
	content: "\25B8";
	color: #7a8699;
	margin-right: 6px;
}

/* Level 3: regular, smaller, muted with a dash marker. */
.toc-l3 > .toc-entry {
	font-size: 0.93em;
	font-weight: normal;
	color: #555;
}

.toc-l3 > .toc-entry::before {
	content: "\2013";
	color: #9aa3b0;
	margin-right: 6px;
}

/* Level 4 and deeper: smallest, italic, lightest, with a dot marker. */
.toc-l4 > .toc-entry,
.toc-l5 > .toc-entry,
.toc-l6 > .toc-entry {
	font-size: 0.88em;
	font-weight: normal;
	font-style: italic;
	color: #777;
}

.toc-l4 > .toc-entry::before,
.toc-l5 > .toc-entry::before,
.toc-l6 > .toc-entry::before {
	content: "\2022";
	color: #b0b7c3;
	margin-right: 6px;
}
`,
		formatMM(margins.Top),
		formatMM(margins.Bottom),
		cssString(footerURI),
		pageNumberCSS(numbering, margins.Right),
		formatMM(margins.Right),
		formatMM(margins.Left),
	)

	return `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<base href="` + assetBaseURL + `">
	<title>Markdown Export</title>
	<style>` + style + `</style>
</head>
<body>
` + body + probeLinks(numbering.Probe) + `
</body>
</html>`
}

// pageNumberCSS returns the CSS printing the page number at the top right.
// Pages listed in numbering.Hidden show no number. When the total page count
// is known, a custom counter style maps those pages to an empty string, because
// Chromium cannot select "the first page of a chapter" in @page rules.
func pageNumberCSS(numbering pageNumbering, rightMM float64) string {
	var css strings.Builder
	counter := "counter(page)"

	if numbering.Total > 0 {
		css.WriteString("@counter-style page-number {\n\tsystem: fixed;\n\tsymbols:")
		for page := 1; page <= numbering.Total; page++ {
			if numbering.Hidden[page] {
				css.WriteString(` ""`)
			} else {
				fmt.Fprintf(&css, ` "%d"`, page)
			}
		}
		css.WriteString(";\n\tfallback: decimal;\n}\n")
		counter = "counter(page, page-number)"
	}

	fmt.Fprintf(&css, `@page {
	@top-right {
		content: %s;
		text-align: right;
		vertical-align: middle;
		padding-right: %smm;
		font-family: Aptos, Arial, sans-serif;
		font-size: 10pt;
		color: #555;
	}
}
`, counter, formatMM(rightMM))

	if numbering.Hidden[1] {
		css.WriteString("@page :first {\n\t@top-right {\n\t\tcontent: none;\n\t}\n}\n")
	}

	return css.String()
}

var (
	tableRe = regexp.MustCompile(`(?s)<table>.*?</table>`)
	theadRe = regexp.MustCompile(`(?s)<thead>\s*(<tr>.*?</tr>)\s*</thead>`)
	tbodyRe = regexp.MustCompile(`(?s)<tbody>(.*?)</tbody>`)
	rowRe   = regexp.MustCompile(`(?s)<tr>.*?</tr>`)
)

// tableRowID returns the id given to a body row while measuring pages.
func tableRowID(table, row int) string {
	return fmt.Sprintf("tbl%d-r%d", table, row)
}

// tableRowCounts returns the number of body rows of every table.
func tableRowCounts(html string) []int {
	var counts []int
	for _, table := range tableRe.FindAllString(html, -1) {
		count := 0
		if body := tbodyRe.FindStringSubmatch(table); body != nil {
			count = len(rowRe.FindAllString(body[1], -1))
		}
		counts = append(counts, count)
	}
	return counts
}

// transformTables prepares every table for printing. Body rows get an odd/even
// class (the stripes must ignore inserted rows). With pages == nil, rows also
// get an id so their page can be measured. With measured pages, the repeated
// header is replaced by explicit "continuation header" rows inserted before the
// first row of each following page, so they can be colored differently from the
// main header. They have the same height as the header, so pages break exactly
// as they did while measuring.
func transformTables(html string, pages map[string]int) string {
	index := 0
	changed := false

	out := tableRe.ReplaceAllStringFunc(html, func(table string) string {
		current := index
		index++

		head := theadRe.FindStringSubmatch(table)
		body := tbodyRe.FindStringSubmatchIndex(table)
		if head == nil || body == nil {
			return table
		}
		changed = true

		continuation := strings.Replace(head[1], "<tr>", `<tr class="table-cont-header">`, 1)

		row := 0
		rows := rowRe.ReplaceAllStringFunc(table[body[2]:body[3]], func(tr string) string {
			number := row
			row++

			class := "tr-odd"
			if number%2 == 1 {
				class = "tr-even"
			}

			id := tableRowID(current, number)
			attrs := ` class="` + class + `"`
			prefix := ""

			if pages == nil {
				attrs += ` id="` + id + `"`
			} else if number > 0 {
				page, ok := pages[id]
				previous, okPrevious := pages[tableRowID(current, number-1)]
				if ok && okPrevious && page > previous {
					prefix = continuation
				}
			}

			return prefix + "<tr" + attrs + ">" + strings.TrimPrefix(tr, "<tr>")
		})

		return table[:body[2]] + rows + table[body[3]:]
	})

	if changed && pages != nil {
		// Headers are inserted explicitly, so stop Chromium repeating them.
		out = "<style>thead { display: table-row-group; }</style>\n" + out
	}

	return out
}

// probeLinks returns invisible links to the given heading ids. Chromium only
// records a PDF destination for anchors that are linked to.
func probeLinks(ids []string) string {
	if len(ids) == 0 {
		return ""
	}

	var buf strings.Builder
	buf.WriteString(`<div style="position:absolute;top:0;left:0;width:0;height:0;overflow:hidden">`)
	for _, id := range ids {
		buf.WriteString(`<a href="#` + htmlEscape(id) + `">.</a>`)
	}
	buf.WriteString("</div>")

	return buf.String()
}

// probeHeadingPages renders the document once and returns the total page count
// and the page on which each of the given heading ids is.
func probeHeadingPages(
	browser string,
	doc convertedDoc,
	ids []string,
	footer string,
	assetBase string,
	margins PageMargins,
	tmp string,
	timeout time.Duration,
) (int, map[string]int, error) {
	probe := buildHTMLDocument(doc.html(nil), footer, assetBase, margins, pageNumbering{Probe: ids})

	htmlPath := filepath.Join(tmp, "probe.html")
	if err := os.WriteFile(htmlPath, []byte(probe), 0600); err != nil {
		return 0, nil, err
	}

	pdfPath := filepath.Join(tmp, "probe.pdf")
	if err := exportPDF(browser, htmlPath, pdfPath, tmp, timeout); err != nil {
		return 0, nil, err
	}

	data, err := os.ReadFile(pdfPath)
	if err != nil {
		return 0, nil, err
	}

	return pdfDestinationPages(data, ids)
}

var (
	pdfRootRe  = regexp.MustCompile(`/Root\s+(\d+)\s+\d+\s+R`)
	pdfPagesRe = regexp.MustCompile(`/Pages\s+(\d+)\s+\d+\s+R`)
	pdfDestsRe = regexp.MustCompile(`/Dests\s+(\d+)\s+\d+\s+R`)
	pdfKidsRe  = regexp.MustCompile(`(?s)/Kids\s*\[([^\]]*)\]`)
	pdfRefRe   = regexp.MustCompile(`(\d+)\s+\d+\s+R`)
)

// pdfObject returns the body of the numbered object, or nil.
func pdfObject(data []byte, number string) []byte {
	marker := []byte("\n" + number + " 0 obj")
	start := bytes.Index(data, marker)
	if start < 0 {
		return nil
	}

	rest := data[start+len(marker):]
	end := bytes.Index(rest, []byte("endobj"))
	if end < 0 {
		return nil
	}

	return rest[:end]
}

// pdfDestinationPages reads a Chromium (Skia) PDF and returns its page count
// and the 1-based page of every named destination in ids (by id).
func pdfDestinationPages(data []byte, ids []string) (int, map[string]int, error) {
	tail := data[max(0, len(data)-8192):]
	roots := pdfRootRe.FindAllSubmatch(tail, -1)
	if len(roots) == 0 {
		return 0, nil, errors.New("PDF catalog not found")
	}

	catalog := pdfObject(data, string(roots[len(roots)-1][1]))
	pagesRef := pdfPagesRe.FindSubmatch(catalog)
	destsRef := pdfDestsRe.FindSubmatch(catalog)
	if pagesRef == nil {
		return 0, nil, errors.New("PDF page tree not found")
	}
	if destsRef == nil {
		return 0, nil, errors.New("PDF has no named destinations")
	}

	pageIndex := map[string]int{}
	var walk func(number string, depth int)
	walk = func(number string, depth int) {
		kids := pdfKidsRe.FindSubmatch(pdfObject(data, number))
		if kids == nil || depth > 32 {
			pageIndex[number] = len(pageIndex) + 1
			return
		}
		for _, ref := range pdfRefRe.FindAllSubmatch(kids[1], -1) {
			walk(string(ref[1]), depth+1)
		}
	}
	walk(string(pagesRef[1]), 0)

	dests := pdfObject(data, string(destsRef[1]))
	pages := map[string]int{}
	for _, id := range ids {
		re := regexp.MustCompile("/" + regexp.QuoteMeta(pdfName(id)) + `\s*\[\s*(\d+)\s+\d+\s+R`)
		match := re.FindSubmatch(dests)
		if match == nil {
			continue
		}
		if page, ok := pageIndex[string(match[1])]; ok {
			pages[id] = page
		}
	}

	if len(pageIndex) == 0 {
		return 0, nil, errors.New("PDF has no pages")
	}

	return len(pageIndex), pages, nil
}

// pdfName encodes a string as the body of a PDF name object.
func pdfName(value string) string {
	var buf strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c <= 0x20 || c >= 0x7f || strings.IndexByte("()<>[]{}/%#", c) >= 0 {
			fmt.Fprintf(&buf, "#%02X", c)
		} else {
			buf.WriteByte(c)
		}
	}
	return buf.String()
}

// formatMM formats a margin value without unnecessary trailing zeros.
func formatMM(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// cssString escapes a string used inside a CSS quoted string.
func cssString(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", "",
		"\r", "",
	)
	return replacer.Replace(value)
}

// validateMargins validates the configured page margins.
func validateMargins(margins PageMargins) error {
	values := map[string]float64{
		"top":    margins.Top,
		"right":  margins.Right,
		"bottom": margins.Bottom,
		"left":   margins.Left,
	}

	for name, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("invalid %s margin: %v", name, value)
		}
	}

	return nil
}

// resolveFooterPath finds the footer image.
func resolveFooterPath(explicit string) (string, error) {
	if explicit != "" {
		path, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}

		if err := validateFile(path); err != nil {
			return "", err
		}

		return path, nil
	}

	candidates := []string{
		installedFooterPath,
		legacyFooterPath,
		localFooterPath,
	}

	for _, candidate := range candidates {
		path, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}

		if err := validateFile(path); err == nil {
			return path, nil
		}
	}

	return "", errors.New("footer image not found")
}

// replaceInstalledFooter atomically replaces the installed footer image.
func replaceInstalledFooter(source string) error {
	sourcePath, err := filepath.Abs(source)
	if err != nil {
		return err
	}

	if err := validateFile(sourcePath); err != nil {
		return fmt.Errorf("source footer: %w", err)
	}

	targetPath, err := filepath.Abs(installedFooterPath)
	if err != nil {
		return err
	}

	if samePath(sourcePath, targetPath) {
		return nil
	}

	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("read source footer: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("create footer directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(targetPath), ".footer-*")
	if err != nil {
		return fmt.Errorf("create temporary footer: %w", err)
	}

	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write footer: %w", err)
	}

	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("set footer permissions: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary footer: %w", err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("install footer: %w", err)
	}

	return nil
}

// findChromium detects an installed Chromium or Chrome executable.
func findChromium() (string, error) {
	browsers := []string{
		"chromium",
		"chromium-browser",
		"google-chrome",
		"google-chrome-stable",
	}

	for _, name := range browsers {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}

	return "", errors.New("Chromium/Chrome not found on PATH")
}

// exportPDF executes Chromium to print an HTML document to PDF.
func exportPDF(
	browser string,
	htmlPath string,
	output string,
	tmp string,
	timeout time.Duration,
) error {
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	htmlURL := "file://" + filepath.ToSlash(htmlPath)

	args := []string{
		"--headless",
		"--disable-gpu",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--allow-file-access-from-files",
		"--no-pdf-header-footer",
		"--user-data-dir=" + filepath.Join(tmp, "chrome-profile"),
		"--print-to-pdf=" + output,
		htmlURL,
	}

	cmd := exec.CommandContext(ctx, browser, args...)

	data, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("Chromium timed out after %s: %w", timeout, ctx.Err())
	}

	if err != nil {
		return fmt.Errorf("Chromium failed: %w\n%s", err, data)
	}

	return nil
}

// validatePDF checks whether the output is a non-empty PDF file.
func validatePDF(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() {
		return errors.New("output is not a regular file")
	}

	if info.Size() < 5 {
		return errors.New("PDF output is empty or incomplete")
	}

	header := make([]byte, 5)
	if _, err := io.ReadFull(file, header); err != nil {
		return err
	}

	if string(header) != "%PDF-" {
		return errors.New("invalid PDF file signature")
	}

	return nil
}

// validateFile checks whether a path points to a regular file.
func validateFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}

	return nil
}

// samePath compares two absolute file paths.
func samePath(first, second string) bool {
	first, err1 := filepath.Abs(first)
	second, err2 := filepath.Abs(second)

	if err1 != nil || err2 != nil {
		return false
	}

	return filepath.Clean(first) == filepath.Clean(second)
}
