package mailbox

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestWalkAttachmentParts_MultipartMixed(t *testing.T) {
	// multipart/mixed
	//   1 text/plain
	//   2 application/pdf "invoice.pdf" (attachment)
	//   3 image/png       "diagram.png" (inline → ignored when no
	//                                    attachment disposition + no name
	//                                    in Content-Type; but here we set
	//                                    name so it counts)
	bs := &imap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []imap.BodyStructure{
			&imap.BodyStructureSinglePart{
				Type: "text", Subtype: "plain", Size: 120,
			},
			&imap.BodyStructureSinglePart{
				Type: "application", Subtype: "pdf", Size: 8192,
				Extended: &imap.BodyStructureSinglePartExt{
					Disposition: &imap.BodyStructureDisposition{
						Value:  "attachment",
						Params: map[string]string{"filename": "invoice.pdf"},
					},
				},
			},
			&imap.BodyStructureSinglePart{
				Type: "image", Subtype: "png", Size: 4096,
				Params: map[string]string{"name": "diagram.png"},
			},
		},
	}
	parts := walkAttachmentParts(bs)
	if len(parts) != 2 {
		t.Fatalf("expected 2 attachments, got %d (%+v)", len(parts), parts)
	}
	if parts[0].Filename != "invoice.pdf" || parts[0].MIME != "application/pdf" {
		t.Fatalf("attachment 0 wrong: %+v", parts[0])
	}
	if parts[0].MIMEPartID != "2" {
		t.Fatalf("attachment 0 part id should be \"2\", got %q", parts[0].MIMEPartID)
	}
	if parts[1].Filename != "diagram.png" || parts[1].MIME != "image/png" {
		t.Fatalf("attachment 1 wrong: %+v", parts[1])
	}
	if parts[1].MIMEPartID != "3" {
		t.Fatalf("attachment 1 part id should be \"3\", got %q", parts[1].MIMEPartID)
	}
}

func TestWalkAttachmentParts_NestedMultipart(t *testing.T) {
	// multipart/mixed
	//   1 multipart/alternative
	//     1.1 text/plain
	//     1.2 text/html
	//   2 application/zip "logs.zip"
	bs := &imap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []imap.BodyStructure{
			&imap.BodyStructureMultiPart{
				Subtype: "alternative",
				Children: []imap.BodyStructure{
					&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: 100},
					&imap.BodyStructureSinglePart{Type: "text", Subtype: "html", Size: 200},
				},
			},
			&imap.BodyStructureSinglePart{
				Type: "application", Subtype: "zip", Size: 65536,
				Extended: &imap.BodyStructureSinglePartExt{
					Disposition: &imap.BodyStructureDisposition{
						Value:  "attachment",
						Params: map[string]string{"filename": "logs.zip"},
					},
				},
			},
		},
	}
	parts := walkAttachmentParts(bs)
	if len(parts) != 1 {
		t.Fatalf("expected 1 attachment (text alternatives don't count), got %d", len(parts))
	}
	if parts[0].MIMEPartID != "2" {
		t.Fatalf("nested part id should be \"2\" not \"1.x\", got %q", parts[0].MIMEPartID)
	}
}

func TestWalkAttachmentParts_TextOnlyHasNoAttachments(t *testing.T) {
	bs := &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: 50}
	if parts := walkAttachmentParts(bs); len(parts) != 0 {
		t.Fatalf("plaintext-only mail should not produce attachments, got %+v", parts)
	}
}

func TestFormatPath(t *testing.T) {
	if got := formatPath(nil); got != "1" {
		t.Fatalf("nil path → %q want %q", got, "1")
	}
	if got := formatPath([]int{2}); got != "2" {
		t.Fatalf("single → %q", got)
	}
	if got := formatPath([]int{2, 1, 4}); got != "2.1.4" {
		t.Fatalf("nested → %q", got)
	}
}
