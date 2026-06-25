package templ

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestMessageView_WebhookRendersImageAttachment guards the inbound-media render:
// a KindWebhook bubble (an external bridge's image-bearing post) must render its
// attachments, not just the body. Regression test for the webhook branch having
// omitted the MessageAttachments block.
func TestMessageView_WebhookRendersImageAttachment(t *testing.T) {
	m := MsgView{
		ID:         "m1",
		Kind:       MsgKindWebhook,
		AuthorName: "Bridge",
		BodyHTML:   "saint",
		Attachments: []AttachmentView{{
			ID: "a1", URL: "/uploads/up1?exp=1&sig=x",
			MIME: "image/png", Kind: "image", Filename: "shot.png",
		}},
	}
	var buf bytes.Buffer
	if err := MessageView(m, false, "", "", "main", true).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "msg-attach-image") {
		t.Fatalf("webhook bubble must render image attachment, got:\n%s", out)
	}
	if !strings.Contains(out, "/uploads/up1") {
		t.Fatalf("attachment URL missing, got:\n%s", out)
	}
}

// TestMessageGroup_DeletedRendersNothing asserts a soft-deleted message emits no
// bubble — no <article>, no body, no "[message removed]" / "[deleted by mod]"
// placeholder — even for a mod viewer (the worst case: old code showed the real
// body to mods). MessageGroup is MessageView's only caller and the template-side
// guard over the query-level filter in listBefore; deleted chat content must be
// invisible to everyone.
func TestMessageGroup_DeletedRendersNothing(t *testing.T) {
	g := MsgGroup{
		AuthorID: "u-mallory",
		Kind:     MsgKindUser,
		Messages: []MsgView{{
			ID:         "m1",
			Kind:       MsgKindUser,
			AuthorName: "Mallory",
			BodyHTML:   "secret content that was removed",
			Deleted:    true,
		}},
	}
	var buf bytes.Buffer
	if err := MessageGroup(g, true, "u-mod", "Mod", "main").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "secret content") || strings.Contains(out, "message removed") || strings.Contains(out, "deleted by mod") {
		t.Fatalf("deleted message leaked content/placeholder, got:\n%s", out)
	}
	if strings.Contains(out, `id="msg-m1"`) {
		t.Fatalf("deleted message must not render its <article> bubble, got:\n%s", out)
	}
}
