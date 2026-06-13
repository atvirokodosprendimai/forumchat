---
name: spec-invites-admin-add-by-email-and-join-page
status: draft
type: spec
---

# Invites — admin add-by-email and branded join page

## Claim

Per-community admins can add a person to their community by typing an email — "click, click, edit, done". The admin doesn't generate a code, doesn't dispatch the share, and doesn't approve afterwards. The flow produces:

- An existing-account hit → membership row inserted, pre-approved, member sees the community on next login.
- A new email → placeholder `users` row + pre-approved membership + one-time signup token. Admin gets a per-community join URL to share out-of-band. Invitee opens it, sets a password, lands in `/c/{slug}/chat`.

A branded **join page** lives at `/c/{slug}/join?code=...`. Logged-in viewers see "Join {community}" and one button. Logged-out viewers see "Sign in OR set password" depending on whether the token resolves to a placeholder user.

The existing reusable invite-code flow (`POST /c/{slug}/admin/invite` + `/register?code=`) stays in place — that's the open-share path. Add-by-email is the targeted path.

## Behaviours

### Admin → "Add member by email"

- New section on `/c/{slug}/admin` above the existing Pending list.
- Single email input + Role dropdown (member / moderator) + "Add" button.
- On click:
  - Server validates email.
  - Looks up user by email.
  - **If user exists**:
    - If they already have a membership in this community, surface an inline error.
    - Else: insert `memberships` row with `approved_at = now`, `role`. Show "Added — they'll see the community on next sign-in".
  - **If user doesn't exist**:
    - Insert `users` row with `email`, empty `password_hash`, `status = 'invited'`.
    - Insert `memberships` row with `approved_at = now`, `role`.
    - Mint a one-time signup token bound to that user; insert `signup_tokens` row.
    - Render a copy-able join URL: `https://{baseURL}/c/{slug}/join?code={token}`.
- Admin can copy the URL and send it however they like (Slack, email, paper). No SMTP dependency in v1.

### Join page (`/c/{slug}/join?code={token}`)

- Page resolves the token; if invalid/expired/used → "this invite has expired" panel + link to login.
- Token valid:
  - Look up the target community by slug.
  - Look up the target user via the token's `user_id`.
  - **Branch on viewer state**:
    - **Viewer is the right user (logged in)** → "Welcome back to {community}" + Join button. Click → mark token used, redirect to `/c/{slug}/chat`. (Membership already exists; we just consume the token.)
    - **Viewer is a different logged-in user** → "Sign out to accept this invite for {email}" + sign-out button.
    - **Not signed in AND user has password_hash** → "Sign in to join {community}" + login form prefilled with email.
    - **Not signed in AND user is a placeholder (status=invited)** → "Set your password and join {community}" + password form. Submit → set password_hash, set status=active, mark token used, log them in, redirect to `/c/{slug}/chat`.

### Token lifecycle

- 7-day TTL (configurable).
- Single-use: marked `used_at` when consumed.
- A placeholder user without a consumed token cannot log in (their `password_hash` is empty). Re-inviting (admin re-adding the same email) mints a new token for the existing placeholder.

### Coexistence with the existing invite-code path

- Existing `invite_codes` table is **not** modified. That path is for self-service open invites.
- New `signup_tokens` table is for targeted add-by-email.
- The `/c/{slug}/join?code=...` URL uses `signup_tokens.token`. Old reusable codes still go through `/register?code=...`. Two URLs, two intents.

## Schema

```sql
ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active','invited','disabled'));

CREATE TABLE signup_tokens (
    token         TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    expires_at    INTEGER NOT NULL,
    used_at       INTEGER,
    created_at    INTEGER NOT NULL
);
CREATE INDEX idx_signup_tokens_user ON signup_tokens(user_id);
```

Token format: 32 hex chars from `crypto/rand` (128 bits).

Note: `users.status` may already exist (check `internal/auth/user.go` — there is a `UserStatus` const). If yes, just add the `'invited'` value to the allowed set. If the migration would conflict with existing rows, default to `'active'` for all current users (already the default).

## Interactions

Affects:

- `internal/auth/` — new repo methods: `CreateInvitedUser`, `ActivateUser`, `MintSignupToken`, `ConsumeSignupToken`, `SignupTokenByValue`.
- `internal/admin/admin.go` — `PostAddMember` handler.
- `web/templ/admin.templ` — "Add member by email" form section above Pending.
- New `internal/invites/` package (or fold into `internal/auth/`) — `GetJoin` and `PostJoin` handlers.
- `web/templ/join.templ` — branded join page (logged-in confirm, set-password, sign-in branches).
- `cmd/app/main.go` — route `/c/{slug}/join` (publicly accessible; doesn't go through `RequireMember` because the whole point is to admit a non-member).
- Migration `00007_signup_tokens.sql`.

Depends on:

- Per-community routing (`/c/{slug}`).
- `community.LoadCommunity` middleware can run on the join page so the templ can render the community name; `RequireMember` does NOT.
- `auth.Identity` for the "viewer is logged in" branches.

## Verification

- Admin types email of existing user X → form clears + success banner. X signs in → community appears on dashboard, lands in /c/{slug}/chat with the right role.
- Admin types brand-new email → join URL appears, copyable. Open URL in incognito → "Set password" form. Submit → land in chat. Token marked used.
- Same URL opened again → "expired" panel.
- Logged-in user opens URL meant for someone else → "sign out to accept" panel.
- Admin re-adds the same email when invitee hasn't joined → new token minted; old URL becomes invalid (the new one isn't used either, but resolves to same user; membership already exists so insert is no-op).
- Existing `/register?code=...` invite-code path still works (not regressed).

## Friction / Trade-offs

- Two parallel admit paths (invite code vs add-by-email) add UI surface. The admin page makes the intent clear by section headings.
- Placeholder users (`status='invited'`, no password) need to be excluded from login flow; `auth.Service.Login` must reject `status != 'active'`. Need to verify that — possible existing logic, or add it.
- Out-of-band sharing puts onus on the admin to deliver. Acceptable for v1; SMTP wiring for direct delivery is a future improvement, NOT in scope here.
- Token format is opaque hex — no human-readable. Trade-off for security; admin doesn't dictate the URL.

## Future

- Direct SMTP delivery — server emails the join URL on click.
- Bulk add: paste a list of emails.
- Per-invite expiry override on the admin form.
- Decline / cancel-invite from admin UI.
- Add-by-email for chat-bot / API integration.

## Status

draft — ready for `/eidos:plan`.
