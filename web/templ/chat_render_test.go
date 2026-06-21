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
