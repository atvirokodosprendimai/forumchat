---
tldr: Build per-project Discussions (forum-style threads + replies, image attachments, guest read+write) in 3 phases. Each phase commits separately; merge to main after PD3.
status: active
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

### PD2 — Replies + quoted-reply + edit-grace — status: open

Goal: members and guests reply on any thread, quote each other's replies, edit within grace, soft-delete own.

1. [ ] `discussions_repo.go` — ListReplies, ReplyByID, InsertReply, UpdateReply, SoftDeleteReply
2. [ ] `discussions_service.go` — AddReply, UpdateReply (grace), DeleteReply; bump thread last_activity on add
3. [ ] `discussions_handler.go` — PostReply, PostReplyEdit, PostReplyDelete; all redirect-after-success (same pattern as issues page)
4. [ ] Templ — `ProjectDiscussionThreadPage` extended with reply list, reply form, quote-this-reply affordance
5. [ ] Quoted-reply renders as a `<blockquote>` block above the new reply body — markdown-rendered server-side from the source reply's `body_md`

Verification: guest replies to admin's reply → quoted block visible above the new reply.

### PD3 — Image attachments + spec sync + merge — status: open

Goal: paste/drag image into thread or reply body, image renders inline. Spec status → shipped. Merge to main.

1. [ ] Reuse the existing paste-image flow that chat + forum already use (signal `image_data` + JS paste handler) for the thread body composer + reply body composer
2. [ ] Server: when `body_md` contains a `data:` URL, decode via `uploads.Store.SaveDataURL` + rewrite to a signed `/uploads/{id}?sig=...` URL before saving body_md/html. Same flow chat/forum already use.
3. [ ] Verify guest uploads work (uploader_user_id NULL path + project-creator-as-owner trick from commit `cd149de`)
4. [ ] Spec status: draft → shipped
5. [ ] Plan status: active → completed
6. [ ] Merge to main as one tagged step

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
