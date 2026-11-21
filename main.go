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
	// tocMaxLevel is the deepest heading level included in the List of Content.
	tocMaxLevel = 3
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

	body, err := convertMarkdown(markdown)
	if err != nil {
		return fmt.Errorf("convert Markdown: %w", err)
	}

	document := buildHTMLDocument(body, footer, filepath.Dir(input), margins)

	htmlPath := filepath.Join(tmp, "document.html")
	if err := os.WriteFile(htmlPath, []byte(document), 0600); err != nil {
		return fmt.Errorf("write HTML document: %w", err)
	}

	browser, err := findChromium()
	if err != nil {
		return err
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

// convertMarkdown converts Markdown content into HTML. A paragraph consisting
// only of "!list-of-content" is replaced by a generated List of Content.
func convertMarkdown(source []byte) (string, error) {
	doc := markdownParser.Parser().Parse(text.NewReader(source))

	var entries []tocEntry
	hasMarker := false

	err := ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		switch n := node.(type) {
		case *ast.Heading:
			if n.Level <= tocMaxLevel {
				var id string
				if value, ok := n.AttributeString("id"); ok {
					if b, ok := value.([]byte); ok {
						id = string(b)
					}
				}
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
		return "", err
	}

	var output bytes.Buffer
	if err := markdownParser.Renderer().Render(&output, source, doc); err != nil {
		return "", err
	}

	html := output.String()
	if hasMarker {
		html = strings.ReplaceAll(html, "<p>"+tocMarker+"</p>", buildTOC(entries))
	}

	return html, nil
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
func buildTOC(entries []tocEntry) string {
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

		title := htmlEscape(entry.Title)
		if entry.ID != "" {
			buf.WriteString(`<a class="toc-entry" href="#` + htmlEscape(entry.ID) + `">` + title + `</a>`)
		} else {
			buf.WriteString(`<span class="toc-entry">` + title + `</span>`)
		}
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
func buildHTMLDocument(body, footerPath, assetBasePath string, margins PageMargins) string {
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
	margin-bottom: 12px;
}

th, td {
	border: 1px solid #ccc;
	padding: 5px 8px;
	text-align: left;
	vertical-align: top;
}

thead {
	display: table-header-group;
}

tr {
	break-inside: avoid;
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
	display: block;
	padding: 2px 0;
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

/* Level 3 and deeper: regular, smaller, muted with a dash marker. */
.toc-l3 > .toc-entry,
.toc-l4 > .toc-entry,
.toc-l5 > .toc-entry,
.toc-l6 > .toc-entry {
	font-size: 0.93em;
	font-weight: normal;
	color: #555;
}

.toc-l3 > .toc-entry::before,
.toc-l4 > .toc-entry::before,
.toc-l5 > .toc-entry::before,
.toc-l6 > .toc-entry::before {
	content: "\2013";
	color: #9aa3b0;
	margin-right: 6px;
}
`,
		formatMM(margins.Top),
		formatMM(margins.Bottom),
		cssString(footerURI),
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
` + body + `
</body>
</html>`
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
