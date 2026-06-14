package templ

import (
	"crypto/sha1"
	"encoding/hex"
	"io"
	"os"
	"sync"
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
	f, err := os.Open("./web/static/" + name)
	if err != nil {
		return "dev"
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "dev"
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}
