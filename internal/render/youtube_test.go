package render_test

import (
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

const ytID = "dQw4w9WgXcQ"

// embed runs the real write+display pipeline: RenderMarkdown autolinks the bare
// URL exactly as bluemonday emits it, then EmbedYouTube transforms the anchor —
// so the test exercises the regex against real sanitizer output, not a hand-
// crafted anchor.
func embed(t *testing.T, md string) string {
	t.Helper()
	html, err := render.RenderMarkdown(md)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return render.EmbedYouTube(html)
}

func TestEmbedYouTube_Facade(t *testing.T) {
	t.Parallel()
	urls := []string{
		"https://www.youtube.com/watch?v=" + ytID,
		"https://youtu.be/" + ytID,
		"https://www.youtube.com/shorts/" + ytID,
		"https://youtube.com/embed/" + ytID,
		"https://www.youtube.com/watch?list=PLx&v=" + ytID + "&t=30",
		"https://m.youtube.com/watch?v=" + ytID,
	}
	for _, u := range urls {
		t.Run(u, func(t *testing.T) {
			out := embed(t, u)
			if !strings.Contains(out, `class="yt-embed"`) {
				t.Fatalf("expected facade for %q, got: %s", u, out)
			}
			if !strings.Contains(out, "i.ytimg.com/vi/"+ytID+"/hqdefault.jpg") {
				t.Fatalf("expected thumbnail for id %q, got: %s", ytID, out)
			}
			if !strings.Contains(out, "$_yt_id='"+ytID+"'") {
				t.Fatalf("expected click sets _yt_id=%q, got: %s", ytID, out)
			}
		})
	}
}

func TestEmbedYouTube_NonYouTubeUntouched(t *testing.T) {
	t.Parallel()
	out := embed(t, "https://example.com/watch?v=notyoutube")
	if strings.Contains(out, "yt-embed") {
		t.Fatalf("non-youtube link must not embed, got: %s", out)
	}
	if !strings.Contains(out, `href="https://example.com/watch?v=notyoutube"`) {
		t.Fatalf("non-youtube anchor must pass through, got: %s", out)
	}
}

// RichHTML must both wrap an upload image AND embed a YouTube link in one pass.
func TestRichHTML_ComposesUploadAndYouTube(t *testing.T) {
	t.Parallel()
	md := "shot ![](/uploads/abc?sig=x)\n\nclip https://youtu.be/" + ytID
	html, err := render.RenderMarkdown(md)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := render.RichHTML(html)
	if !strings.Contains(out, `class="upload-img-link"`) {
		t.Fatalf("expected upload image wrapped, got: %s", out)
	}
	if !strings.Contains(out, `class="yt-embed"`) {
		t.Fatalf("expected youtube facade, got: %s", out)
	}
}
