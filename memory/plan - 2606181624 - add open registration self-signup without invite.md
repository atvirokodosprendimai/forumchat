---
tldr: Add an "open registration" mode so people can self-register and join the community without an invite code, gated by two global env flags (OPEN_REGISTRATION + OPEN_REGISTRATION_AUTO_APPROVE)
status: active
---

# Plan: Open registration — self-signup without an invite code

## Context

Today `/register` **requires** an invite code: `PostRegister` rejects an empty
`invite_code` (`internal/auth/handlers.go:154`) and `Service.Register` calls
`ConsumeInvite` unconditionally (`internal/auth/service.go:61`). There is no way
for a stranger to join.

This plan adds an **open registration** mode controlled by two global env flags
(decided with the user 2606181624):

- `OPEN_REGISTRATION` (default `false`) — when `true`, the invite code becomes
  optional; an empty code is accepted and the user joins the bootstrap community.
- `OPEN_REGISTRATION_AUTO_APPROVE` (default `false`) — only meaningful when open
  registration is on. `false` → new members land in the **existing pending
  approval queue** (`approved_at = NULL` → `/pending` → admin approves).
  `true` → instant access (`approved_at = now()` stamped at verify).

Both default off, so the current invite-only behaviour is unchanged unless an
operator opts in. No DB migration is needed — the approval queue, public flag,
`/pending` page, and admin approve flow already exist.

Key facts established while scoping:
- Membership is created in `Service.Verify` (`service.go:190`), NOT in
  `Register`. `ApprovedAt` is currently never set → today every verified user
  (invited or not) hits the pending queue. Auto-approve = set `m.ApprovedAt` here.
- The community a user joins at verify is always the handler's bootstrap
  `h.CommunityID` (`Verify(ctx, token, h.CommunityID)`). The invite's
  `CommunityID` is only used for the `Register` result/log, so the no-invite
  path needs no new community source.
- Next free migration would be 00033, but this plan needs none.

Related specs (extends, not replaced):
- Spec: [[spec - forumchat - community web app with realtime chat and forum threads]]
- Spec: [[spec - invites - admin-add-by-email-and-join-page]]

> Note: no dedicated spec exists for open registration. The change is a small,
> config-gated variation of the existing register flow, so this plan proceeds
> directly. If the behaviour grows (per-community toggle, captcha, domain
> allowlist), promote it to its own `/eidos:spec` first.

## Phases

### Phase 1 - Config + wiring (flags exist, default off = no behaviour change) - status: completed

1. [x] Add the two flags to `internal/config/config.go`
   - `OpenRegistration bool` → `env:"OPEN_REGISTRATION" envDefault:"false"`
   - `OpenRegistrationAutoApprove bool` → `env:"OPEN_REGISTRATION_AUTO_APPROVE" envDefault:"false"`
   - => placed after `CommunityName` with doc comments
2. [x] Add fields to `auth.Service` and wire from config in `cmd/app/main.go`
   - => `OpenRegistration` + `OpenRegistrationAutoApprove` added to `Service`
     struct; wired in `svc := &auth.Service{...}` from `cfg.*`
   - => `CGO_ENABLED=0 go build ./cmd/app` green; both flags default false

### Phase 2 - Backend: optional invite + approval decision - status: open

1. [ ] Make the invite optional in `Service.Register`
   - when `in.InviteCode == ""`:
     - if `s.OpenRegistration` → skip `ConsumeInvite`, return result with
       `CommunityID` left as the bootstrap (or empty — handler doesn't read it)
     - else → return a new typed `ErrInviteRequired`
   - when `in.InviteCode != ""` → consume as today (unchanged path)
2. [ ] Auto-approve at verify when configured
   - in `Service.Verify`, set `m.ApprovedAt = &now` when
     `s.OpenRegistration && s.OpenRegistrationAutoApprove`; otherwise leave nil
     (current pending-queue behaviour)
   - gate on BOTH flags so enabling auto-approve alone (open reg off) never
     changes invite-only behaviour
3. [ ] Relax the handler validation in `PostRegister`
   - require email + password always; require invite **only** when
     `!h.Svc.OpenRegistration` (uppercase/trim invite as today)
   - add an `ErrInviteRequired` case to `registerErrMsg`
   - verify: unit test register-with-no-invite succeeds when flag on (Phase 4)

### Phase 3 - Frontend: register form adapts to mode (visible result) - status: open

1. [ ] Thread the flag into the register page
   - `GetRegister` passes `h.Svc.OpenRegistration` to `RegisterPage(openReg bool)`
2. [ ] Update `web/templ/auth.templ` `RegisterPage`
   - when `openReg`: render the invite field as **optional** — collapse it behind
     a `<details>`/"Have an invite code?" affordance so the default form is just
     email + password (clean self-signup UX); label "Invite code (optional)"
   - when `!openReg`: keep the current required "Invite code" field unchanged
   - run `make gen` (templ → `_templ.go`)
   - verify (manual smoke): run with `OPEN_REGISTRATION=true`, register with no
     invite → "check your email" done fragment → after verify land on `/pending`;
     re-run with `OPEN_REGISTRATION_AUTO_APPROVE=true` → after verify get full
     access (no `/pending`)

### Phase 4 - Tests + docs - status: open

1. [ ] Add service tests in `internal/auth/service_test.go`
   - open reg on, no invite → `Register` ok; after `Verify`, membership
     `ApprovedAt == nil` (queue) when auto-approve off
   - open reg on + auto-approve on, no invite → after `Verify`,
     `ApprovedAt != nil` (instant)
   - open reg off, no invite → `Register` returns `ErrInviteRequired`
   - existing invite-path tests stay green (regression guard)
2. [ ] Document the flags
   - README env-var table: `OPEN_REGISTRATION`, `OPEN_REGISTRATION_AUTO_APPROVE`
   - AGENTS.md §5b (approval queue): note open registration as the no-invite
     entry path and how it interacts with the queue
   - verify: `go test ./...` green

## Verification

- `make build` + `go test ./...` green.
- Defaults off → behaviour byte-identical to today (invite required, queue gates).
- `OPEN_REGISTRATION=true` (auto-approve off): stranger registers with no invite,
  verifies email, lands in `/pending`; admin sees them in `/admin` queue and
  approves; user then reaches the app.
- `OPEN_REGISTRATION=true` + `OPEN_REGISTRATION_AUTO_APPROVE=true`: same but the
  user reaches the app immediately after verifying, no admin step.
- Invite-based registration still works in every mode (no regression).

## Adjustments

<!-- Plans evolve. Document changes with timestamps. -->

## Progress Log

- 2606181624 — Plan created. Scoped against current register/verify flow;
  confirmed membership+approval happen in `Verify`, no migration needed.
  Decisions (with user): two global env flags, auto-approve gated on open-reg,
  invite path preserved, email verification kept.
- 2606181624 — Phase 1 done. Added `OPEN_REGISTRATION` +
  `OPEN_REGISTRATION_AUTO_APPROVE` to config, `Service` struct fields, wired in
  main.go. Build green, defaults off = no behaviour change.
