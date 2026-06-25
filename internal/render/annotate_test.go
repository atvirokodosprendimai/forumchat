package render

import "strings"

import "testing"

func TestAnnotateBlocks(t *testing.T) {
	in := `<h1>Title</h1>
<p>First para</p>
<ul><li>a</li><li>b</li></ul>`
	out, n := AnnotateBlocks(in)
	if n != 3 {
		t.Fatalf("want 3 top-level blocks, got %d", n)
	}
	for _, want := range []string{`<h1 data-nb="0">`, `<p data-nb="1">`, `<ul data-nb="2">`} {
		if !strings.Contains(out, want) {
			t.Fatalf("annotated html missing %q:\n%s", want, out)
		}
	}
	// nested <li> must NOT be annotated — only top-level blocks.
	if strings.Contains(out, `<li data-nb`) {
		t.Fatalf("nested elements should not be annotated:\n%s", out)
	}
}

func TestAnnotateBlocksEmpty(t *testing.T) {
	out, n := AnnotateBlocks("")
	if n != 0 || out != "" {
		t.Fatalf("empty in → empty out, 0 blocks; got %q, %d", out, n)
	}
}
