---
tldr: Implement admin add-by-email and the branded /c/{slug}/join page. Click-click-edit-done flow with placeholder users + one-time signup tokens. No SMTP in v1 — admin copies the URL.
status: active
---

# Plan: Admin add-by-email + branded join page

## Context

- Spec: [[spec - invites - admin-add-by-email-and-join-page]] (commit `7ac927c`)
- Coexists with existing `invite_codes` flow at `/register?code=...` — not modified.
- Auth already has `UserStatus` enum and SMTP wired (for verification). Reusing both.
- Per-community routing established; `community.LoadCommunity` runs at `/c/{slug}/join` but `RequireMember` does NOT (the whole point is to admit a non-member).

## Phases

### Phase 1 — Schema + repo plumbing — status: completed

Goal: DB ready, no UI yet.

1. [ ] Migration `00007_signup_tokens.sql`
   - Add `users.status` to allowed values: include `'invited'` (verify if column exists; if it already does, drop the column-add — only widen the CHECK).
   - Create `signup_tokens(token, user_id, community_id, expires_at, used_at, created_at)` + index on `user_id`.
2. [ ] `auth.Repo`: `CreateInvitedUser(email)` — inserts placeholder user with empty `password_hash`, `status='invited'`. Returns `User`.
3. [ ] `auth.Repo`: `ActivateUser(userID, passwordHash)` — sets `password_hash`, `status='active'`.
4. [ ] `auth.Repo`: `MintSignupToken(userID, communityID, ttl)` — 32-hex random, inserts row, returns token.
5. [ ] `auth.Repo`: `SignupTokenByValue(token)` — returns row or `ErrNotFound`. Excludes expired + used by default.
6. [ ] `auth.Repo`: `ConsumeSignupToken(token)` — sets `used_at = now()`. Atomic-ish.
7. [ ] Verify `auth.Service.Login` rejects `status != 'active'` (it should already); if not, add the gate.

Verification: `go build ./... && go vet ./...` clean. Migration applies cleanly on fresh DB.

### Phase 2 — Admin "Add member by email" handler — status: completed

Goal: form posts, server creates membership (and placeholder user if needed), returns either success banner or join URL.

1. [ ] `admin.PostAddMember`
   - ReadSignals first, NewSSE after (per the datastar-go pattern).
   - Look up user by email.
   - If exists: check membership; if present → ErrorFragment("am-error", "Already a member"); else `CreateMembership(approvedAt=now)` → success banner with name.
   - If not exists: `CreateInvitedUser(email)` → `CreateMembership(approvedAt=now, role)` → `MintSignupToken(user, community, 7*24h)` → render copy-able URL fragment.
   - PatchSignals to clear `cc_member_email` after success.
2. [ ] Route in admin group: `POST /c/{slug}/admin/add-member`.
3. [ ] New signals on the layout: `am_email`, `am_role`.
4. [ ] Admin templ: section "Add member by email" above Pending.
   - Email input bound to `am_email`.
   - Role dropdown bound to `am_role` (member / moderator).
   - Add button → `@post('/c/' + slug + '/admin/add-member')`.
   - Error / success fragment id `am-result`.

Verification: in admin page, type existing email → success banner; type new email → URL appears, copy-able; type already-member email → error.

### Phase 3 — Branded join page (logged-in branches) — status: completed

Goal: `/c/{slug}/join?code=...` renders for the four viewer states.

1. [ ] New `internal/invites/handler.go` with `Handler` struct (AuthRepo + Communities + Sessions).
2. [ ] `Handler.GetJoin`
   - Resolve community from URL (middleware does it).
   - Read `code` from query.
   - Look up token; if expired/used/missing → render `JoinExpired(viewer, community)`.
   - Look up target user (by `token.user_id`).
   - Branch:
     - viewer is target user → `JoinConfirm(viewer, community)`.
     - viewer is different logged-in user → `JoinWrongUser(viewer, community, target.email)`.
     - viewer not logged-in AND target has password → `JoinLogin(community, target.email, code)`.
     - viewer not logged-in AND target is placeholder → `JoinSetPassword(community, target.email, code)`.
3. [ ] `Handler.PostJoinConfirm` — same-user logged-in click of "Join". Validate identity matches token target. ConsumeSignupToken. Redirect to `/c/{slug}/chat`.
4. [ ] `Handler.PostJoinSetPassword` — placeholder activation. ReadSignals (password) → hash → ActivateUser → ConsumeSignupToken → log them in → redirect.
5. [ ] `web/templ/join.templ` — branded layout (community name in headline) + four variants as separate `templ` blocks.
6. [ ] Route `/c/{slug}/join` (GET + the two POSTs) — mounted under `LoadCommunity` middleware but OUTSIDE `RequireMember`. Goes under a fresh chi group.
7. [ ] New signals: `join_password`, `join_password_confirm`.

Verification: open URL → expected branch each of: same user logged in / wrong user / no session existing-account / no session placeholder. Each branch's button completes the loop.

### Phase 4 — Polish + UX glue — status: open

1. [ ] Layout: add "Copy URL" button next to the URL fragment via a tiny JS click handler in `paste.js` (`navigator.clipboard.writeText`).
2. [ ] CSS for `.join-page`, `.join-card`, success / error variants.
3. [ ] If feasible: re-mint token if admin re-adds same email (invalidate old). Either invalidate-on-mint or just rely on TTL.
4. [ ] Update `eidos/spec - invites - admin-add-by-email-and-join-page.md` status `draft → implemented`.
5. [ ] CHANGELOG line.

## Verification (overall)

- Existing `/register?code=...` flow continues to work.
- Add-by-email for existing user: instant, no token, login flow unchanged.
- Add-by-email for new user: URL copy → invitee sets password → lands in chat.
- Wrong-user-logged-in path lets them sign out without consuming the token.
- Expired URL returns the expired panel (and doesn't expose user data).

## Adjustments

(Empty — log timestamped reasons here when phases get reshaped.)

## Progress Log

- 2606131953 — plan created from spec.
- 2606132028 — Phases 1+2+3 completed in one push. Migration 00007, signup_tokens repo, CreateInvitedUser + SetPasswordAndActivate + MintSignupToken + SignupTokenByValue + ConsumeSignupToken. Login rejects StatusInvited explicitly. Admin add-by-email handler PostAddMember handles existing/new email branches with copy-able URL fragment. /c/{slug}/join page + JoinExpired/JoinConfirm/JoinWrongUser/JoinLogin/JoinSetPassword branches. PostJoinConfirm + PostJoinSetPassword complete the round trips. Build+vet clean. Phase 4 (CSS polish + spec status flip) deferred.
