package render_test

import (
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// render runs the real write pipeline so the test exercises DownloadableCode
// against actual goldmark+bluemonday output, not a hand-crafted <pre>.
func renderMD(t *testing.T, md string) string {
	t.Helper()
	html, err := render.RenderMarkdown(md)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return html
}

func TestDownloadableCode_HTMLBlock(t *testing.T) {
	t.Parallel()
	out := render.DownloadableCode(renderMD(t, "```html\n<div>hi</div>\n```"))

	for _, want := range []string{
		`class="codeblock"`,
		`<pre><code class="language-html">`, // original block preserved
		`data-ext="html"`,
		`data-mime="text/html"`,
		`data-on:click="window.fcDownloadCode(el)"`,
		`Download .html`,
		`window.fcPreviewCode(el);$_html_open=true`, // HTML blocks get a sandboxed preview
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDownloadableCode_PreviewOnlyForHTML(t *testing.T) {
	t.Parallel()
	out := render.DownloadableCode(renderMD(t, "```go\nfmt.Println(\"hi\")\n```"))
	if strings.Contains(out, "fcPreviewCode") {
		t.Errorf("non-HTML block should not get a preview button:\n%s", out)
	}
}

func TestDownloadableCode_NoLanguageFallsBackToTxt(t *testing.T) {
	t.Parallel()
	out := render.DownloadableCode(renderMD(t, "```\nplain text\n```"))
	if !strings.Contains(out, `data-ext="txt"`) || !strings.Contains(out, `data-mime="text/plain"`) {
		t.Errorf("want txt/text-plain fallback, got:\n%s", out)
	}
}

func TestDownloadableCode_NonCodeUntouched(t *testing.T) {
	t.Parallel()
	in := renderMD(t, "just a **paragraph** with `inline` code, no fence")
	if got := render.DownloadableCode(in); got != in {
		t.Errorf("non-fenced HTML changed:\nin:  %s\nout: %s", in, got)
	}
}

func TestIsHTMLDocument(t *testing.T) {
	t.Parallel()
	docs := []string{
		"<!DOCTYPE html>\n<html lang=\"lt\">...",
		"intro text\n\n<!doctype html>\n<head>",
		"<html><body>x</body></html>",
		"```html\n<!DOCTYPE html><html></html>\n```",
	}
	notDocs := []string{
		"",
		"<div class=\"box\">a snippet, not a document</div>",
		"<style>body{}</style>",
		"plain prose mentioning html",
	}
	for _, s := range docs {
		if !render.IsHTMLDocument(s) {
			t.Errorf("IsHTMLDocument(%q) = false, want true", s)
		}
	}
	for _, s := range notDocs {
		if render.IsHTMLDocument(s) {
			t.Errorf("IsHTMLDocument(%q) = true, want false", s)
		}
	}
}

func TestLooksLikeHTML(t *testing.T) {
	t.Parallel()
	html := []string{
		"```html\n<div>hi</div>\n```",
		"<!doctype html><html></html>",
		"<style>body{color:red}</style>\n<div class=\"box\">x</div>",
		"Here is a page:\n<section><h1>Title</h1></section>",
	}
	notHTML := []string{
		"",
		"just a normal sentence about HTML and CSS",
		"a list:\n- one\n- two",
		"inline `code` and a [link](https://x.dev)",
	}
	for _, s := range html {
		if !render.LooksLikeHTML(s) {
			t.Errorf("LooksLikeHTML(%q) = false, want true", s)
		}
	}
	for _, s := range notHTML {
		if render.LooksLikeHTML(s) {
			t.Errorf("LooksLikeHTML(%q) = true, want false", s)
		}
	}
}

func TestRichHTML_WiresDownloadButton(t *testing.T) {
	t.Parallel()
	out := render.RichHTML(renderMD(t, "```go\nfmt.Println(\"hi\")\n```"))
	if !strings.Contains(out, `data-ext="go"`) || !strings.Contains(out, "class=\"codeblock\"") {
		t.Errorf("RichHTML did not wire DownloadableCode:\n%s", out)
	}
}
