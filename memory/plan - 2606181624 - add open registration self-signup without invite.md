---
tldr: Add an "open registration" mode so people can self-register and join the community without an invite code, gated by two global env flags (OPEN_REGISTRATION + OPEN_REGISTRATION_AUTO_APPROVE)
status: completed
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

### Phase 2 - Backend: optional invite + approval decision - status: completed

1. [x] Make the invite optional in `Service.Register`
   - => empty code: consume skipped when `s.OpenRegistration`, else
     `ErrInviteRequired`; non-empty consumed as before. `communityID` local
     replaces `invite.CommunityID` in the result (empty on open path; handler
     doesn't read it)
   - => added `ErrInviteRequired` sentinel in `internal/auth/errors.go`
2. [x] Auto-approve at verify when configured
   - => `Service.Verify` stamps `m.ApprovedAt = &time.Now()` when
     `s.OpenRegistration && s.OpenRegistrationAutoApprove`; nil otherwise
   - => `ApprovedAt` is `*time.Time` (not int64) — confirmed in `user.go:45`
3. [x] Relax the handler validation in `PostRegister`
   - => email+password always required; invite required only when
     `!h.Svc.OpenRegistration`; added `ErrInviteRequired` case to `registerErrMsg`
   - => `go build ./...` + `go test ./internal/auth/` green

### Phase 3 - Frontend: register form adapts to mode (visible result) - status: completed

1. [x] Thread the flag into the register page
   - => `GetRegister` now calls `webtempl.RegisterPage(h.Svc.OpenRegistration)`
2. [x] Update `web/templ/auth.templ` `RegisterPage`
   - => `RegisterPage(openReg bool)`: when open, invite collapses into a
     `<details><summary>Have an invite code?</summary>` with a placeholder
     "Optional" input; when closed, the required "Invite code" field is unchanged
   - => `make gen` + `go build ./...` green
   - => end-to-end smoke deferred to after Phase 4 tests land (one pass)

### Phase 4 - Tests + docs - status: completed

1. [x] Add service tests in `internal/auth/service_test.go`
   - => `TestRegister_ClosedNoInvite_Refused` (ErrInviteRequired),
     `TestRegister_OpenNoInvite_PendingQueue` (ApprovedAt nil),
     `TestRegister_OpenNoInvite_AutoApprove` (ApprovedAt set) — all pass
   - => `go test ./...` green; existing invite tests unaffected
2. [x] Document the flags
   - => README env-var table + AGENTS.md §5b open-registration note
   - => AGENTS.md is the real file; `CLAUDE.md` is a symlink to it

#### Verification (2606181624 HTTP smoke)

End-to-end smoke (fresh binary, temp DB, high port):
- `OPEN_REGISTRATION=false` (default): POST `/register` with empty invite →
  `<div id="auth-error">Invite code required</div>` ✓
- `OPEN_REGISTRATION=true`: same request → `Check your email` success
  fragment ✓
- Backend approval split (queue vs auto-approve) covered by the service tests
  above.

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
- 2606181624 — Phase 2 done. `Register` invite optional + `ErrInviteRequired`;
  `Verify` auto-approves when both flags set; `PostRegister` validation relaxed.
  Build + auth tests green.
- 2606181624 — Phase 3 done. `RegisterPage(openReg bool)` collapses invite into
  an optional `<details>` when open; `GetRegister` threads the flag.
- 2606181624 — Phase 4 done. 3 service tests (refused / queue / auto-approve),
  README + AGENTS.md docs, HTTP smoke confirms closed vs open. Plan COMPLETED.
