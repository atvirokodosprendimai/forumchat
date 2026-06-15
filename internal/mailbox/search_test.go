package mailbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSearchQueueForViewer_MatchBodyAndAttachment(t *testing.T) {
	repo := setupTestRepo(t)
	cid := communityID(t, repo, "main")
	insertUser(t, repo, "u1")
	f := insertFilter(t, repo, FilterKindDomain, "@acme.com", cid, "u1", false)

	ctx := context.Background()
	acc, _ := repo.EnsureAccount(ctx, AccountConfig{
		Host: "imap.test", Port: 993, Username: "u", Password: "p", TLSMode: "tls",
	})
	folder, _ := repo.UpsertFolder(ctx, acc.ID, "INBOX", 1000)

	insertEmail := func(uid uint32, subject, body string, attNames ...string) string {
		t.Helper()
		id, isNew, err := repo.InsertIngest(ctx, IngestInsert{
			FolderID:    folder.ID,
			UID:         uid,
			UIDValidity: 1000,
			MessageID:   "m" + strings.Join([]string{strings.ReplaceAll(subject, " ", "")}, ""),
			FromAddr:    "alice@acme.com",
			FromName:    "Alice",
			Subject:     subject,
			BodyText:    body,
			ReceivedAt:  time.Now().UTC(),
			CommunityID: cid,
			MatchedFilterID: f.ID,
		})
		if err != nil || !isNew {
			t.Fatalf("ingest: %v isNew=%v", err, isNew)
		}
		parts := make([]ParsedPart, len(attNames))
		for i, n := range attNames {
			parts[i] = ParsedPart{Filename: n, MIME: "application/pdf", SizeBytes: 100, MIMEPartID: "1"}
		}
		if err := repo.InsertAttachments(ctx, id, parts); err != nil {
			t.Fatalf("attachments: %v", err)
		}
		return id
	}

	insertEmail(1, "Quarterly recap", "API documentation rolled out today")
	insertEmail(2, "Random update", "Nothing in this body", "api documentation.doc")
	insertEmail(3, "Hello", "Unrelated note", "invoice.pdf")

	q := QueueQuery{AdminCommunityIDs: []string{cid}}

	hitBody, err := repo.SearchQueueForViewer(ctx, q, "api documentation")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hitBody) != 2 {
		t.Fatalf("query 'api documentation' should match both body + attachment; got %d", len(hitBody))
	}

	hitFile, err := repo.SearchQueueForViewer(ctx, q, "invoice")
	if err != nil {
		t.Fatalf("search invoice: %v", err)
	}
	if len(hitFile) != 1 {
		t.Fatalf("query 'invoice' should match the email with that attachment; got %d", len(hitFile))
	}

	empty, err := repo.SearchQueueForViewer(ctx, q, "")
	if err != nil {
		t.Fatalf("empty query: %v", err)
	}
	if len(empty) != 3 {
		t.Fatalf("empty query should fall through to recent list; got %d", len(empty))
	}
}
