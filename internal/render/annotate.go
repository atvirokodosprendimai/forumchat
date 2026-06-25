package render

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// AnnotateBlocks tags each top-level block of a rendered HTML fragment with a
// data-nb="<i>" attribute (0-based) and returns the annotated HTML plus the
// block count. It is the anchor map for note inline comments: a comment stores
// the block index it attaches to, and a comment whose stored index is >= the
// returned count is "orphaned" (the note was edited and its block moved/went
// away). Runs at display time on already-sanitized HTML; the data-nb attribute
// is our own trusted output. On a parse error the input is returned unchanged
// with a zero count (degrade to no anchors rather than break the page).
func AnnotateBlocks(fragment string) (string, int) {
	ctx := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(strings.NewReader(fragment), ctx)
	if err != nil {
		return fragment, 0
	}
	var buf strings.Builder
	n := 0
	for _, node := range nodes {
		if node.Type == html.ElementNode {
			node.Attr = append(node.Attr, html.Attribute{Key: "data-nb", Val: strconv.Itoa(n)})
			n++
		}
		if err := html.Render(&buf, node); err != nil {
			return fragment, 0
		}
	}
	return buf.String(), n
}
