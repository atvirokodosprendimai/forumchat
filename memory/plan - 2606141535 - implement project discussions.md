---
tldr: Build per-project Discussions (forum-style threads + replies, image attachments, guest read+write) in 3 phases. Each phase commits separately; merge to main after PD3.
status: completed
---

# Plan: Implement project discussions per spec

## Context

- Spec: [[spec - project-discussions - per-project-discussion-threads]]
- Parent: [[spec - projects - per-community-collaborative-projects]]
- Parent: [[spec - project-issues - per-project-issues-with-guest-share-links]] — guest identity, callerIdentity helper, route split pattern, uploads attribution lesson, page-reload pattern are all reused
- Reference (not a dep): `internal/forum` — same thread+post+quote shape

## Phases

### PD1 — Schema + threads CRUD + tab — status: completed

Goal: member or guest opens new thread from the Discussions tab, lands on the thread page (with empty replies list).

1. [x] Migration 00015_project_discussions.sql — both tables ship together (threads + replies)
2. [x] discussions.go — DiscussionThread, DiscussionReply, DiscussionThreadRow (with reply count)
3. [x] discussions_repo.go — ListDiscussionThreads (with reply count subquery), DiscussionThreadByID, InsertDiscussionThread, UpdateDiscussionThread, BumpDiscussionThreadActivity, SoftDeleteDiscussionThread
4. [x] discussions_service.go — CreateDiscussionThread (member + guest via Identity), UpdateDiscussionThread (author + admin, no grace at thread level), DeleteDiscussionThread (author + admin)
5. [x] discussions_handler.go — GetDiscussionsTab, GetDiscussionThread, PostCreateDiscussionThread, PostEditDiscussionThread, PostDeleteDiscussionThread; all redirect-on-success
6. [x] Templ — ProjectDiscussionsPage (list + create form), ProjectDiscussionThreadPage (head + body + edit + delete)
7. [x] Tab strip — "Discussions" sits between Issues and Comments
8. [x] Routes — 5 new entries in the OPEN group
9. [x] CSS — discussion add form + list rows + thread head

Verification: open Discussions tab → "Where should the API key live?" → land on thread page.

### PD2 — Replies + quoted-reply + edit-grace — status: completed

Goal: members and guests reply on any thread, quote each other's replies, edit within grace, soft-delete own.

1. [x] `discussions_repo.go` — ListDiscussionReplies, DiscussionReplyByID, InsertDiscussionReply, UpdateDiscussionReply, SoftDeleteDiscussionReply
2. [x] `discussions_service.go` — AddDiscussionReply (validates quoted_reply_id belongs to the same thread; bumps last_activity), UpdateDiscussionReply (edit-grace + author/admin), DeleteDiscussionReply (author/admin, no grace)
3. [x] `discussions_handler.go` — PostDiscussionReply, PostDiscussionReplyEdit, PostDiscussionReplyDelete; all redirect-on-success
4. [x] Templ — ProjectDiscussionThreadPage extended with reply list (quoted snippet rendered as a `<blockquote>` above the body), inline edit, delete, quote-this-reply button, reply form pinned at the bottom with a "Quoting reply ×" pill when a quote is in progress
5. [x] toDiscussionReplyViews builds a viewer-scoped permission map (CanEdit honors EditGrace; quoted snippet falls back to first 140 chars of the source body)
6. [x] CSS — quoted blockquote, reply rows, reply form, quote-pending pill

Verification: guest replies to admin's reply → quoted block visible above the new reply.

### PD3 — Image attachments + spec sync + merge — status: completed

Goal: paste/drag image into thread or reply body, image renders inline. Spec status → shipped. Merge to main.

1. [x] Thread + reply composer textareas wired `data-on:paste="fcPasteImage(evt, '<image_signal>')"` (existing global paste.js handler) + matching hidden inputs bound to the signal
2. [x] discussionSignals extended with `BodyImage` + `ReplyImage` (data:URL strings)
3. [x] Handler.composeBodyWithImage decodes via uploads.Store.SaveDataURL, builds a `![image](signed-url)` line, prepends it to body markdown before service.Create / service.AddReply
4. [x] Handler.uploaderOwnerID resolves uploads.owner_id: auth user → own id; guest → project-creator id (commit cd149de pattern)
5. [x] Spec status: draft → shipped
6. [x] Plan status: active → completed
7. [x] Merge prep done; merge to main is the final step

Verification: guest pastes a screenshot into a reply → image renders inline for both admin and guest tabs (after their refresh).

## Verification (end-to-end)

- Member creates 3 threads. Newest first by last_activity_at.
- Guest joins via share URL → sees the 3 threads → opens new "Login flow question".
- Member replies "Use bearer token in Authorization header." Guest sees the reply on refresh.
- Guest quotes that reply, pastes a screenshot, posts. Member sees quoted block + image.
- Guest edits own reply within 15 min — "(edited)" stamp appears. Outside grace, Edit button is hidden.
- Member admin deletes the thread → tombstone collapses the entry in the list.

## Progress Log

<!-- Updated after every completed action. -->
