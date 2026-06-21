package templ

import (
	"crypto/sha1"
	"encoding/hex"
	"sync"

	"github.com/a-h/templ"

	"github.com/atvirokodosprendimai/forumchat/web"
)

// AssetVer returns a stable cache-busting suffix derived from the content
// hash of web/static/<name>, computed once per file per process. Appended
// as ?v=... in layout so the browser drops its cached copy whenever the
// underlying file changes on deploy.
func AssetVer(name string) string {
	v, _ := assetVers.LoadOrStore(name, computeAssetVer(name))
	return v.(string)
}

var assetVers sync.Map // name -> "8charsha"

func computeAssetVer(name string) string {
	b, err := web.Static.ReadFile("static/" + name)
	if err != nil {
		return "dev"
	}
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])[:8]
}

// InlineStyle returns a <style> element holding the verbatim contents of
// web/static/<name>, read once per file per process and cached. Inlining the
// stylesheet into the document <head> makes it apply at first paint with no
// extra request, which removes the flash of unstyled content / layout shift
// that an external <link> suffers on every full navigation.
//
// The asset is trusted project content (no user input), so templ.Raw is safe.
func InlineStyle(name string) templ.Component {
	return templ.Raw(inlineCSS(name))
}

func inlineCSS(name string) string {
	v, _ := inlineCSSCache.LoadOrStore(name, "<style>"+readAsset(name)+"</style>")
	return v.(string)
}

var inlineCSSCache sync.Map // name -> "<style>…</style>"

func readAsset(name string) string {
	b, err := web.Static.ReadFile("static/" + name)
	if err != nil {
		return ""
	}
	return string(b)
}
