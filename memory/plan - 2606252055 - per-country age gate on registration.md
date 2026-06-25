---
status: active
created: 2026-06-25
---

# Plan — per-country age gate on registration

## Context

User asked for a `.env` variable for **age verification** whose required age (and
therefore the confirmation text) **depends on the visitor's country** ("13, 14
or whatever"). No age gate exists today; registration is
`web/templ/auth.templ:RegisterPage` → `internal/auth/handlers.go:PostRegister`,
all config in `internal/config/config.go`. Natural attach point is the
open-registration flow (`OpenRegistration` flag, AGENTS.md §5b).

Decisions (confirmed with user via AskUserQuestion):
- **Country source:** CF-IPCountry proxy header prefills a country `<select>`;
  the user can override it. Works behind Cloudflare/proxy and everywhere else.
- **Gate type:** self-attestation **checkbox** ("I confirm I am at least N years
  old"), Register disabled until ticked, **server re-validates** the boolean.
  No DOB collected (less PII). It is honor-based, not identity proof.

One env var, empty = OFF (byte-identical to today), mirroring the codebase idiom
(empty model = inert: ModerationModel, TranslateModel).

`AGE_MIN_BY_COUNTRY="US:13,GB:13,LT:14,DE:16,*:16"` — `<ISO alpha-2>:<age>` pairs
plus `*` catch-all (fallback 16 if `*` omitted).

## Phases

### Phase 1 — agegate policy package — status: open
1. [ ] `internal/agegate`: `Parse`, `Enabled`, `AgeFor(code)`, `Preselect(country)`,
   `Options()` (configured sorted + `*` "Other"), embedded ISO-3166 name table.
   - verify: `go test ./internal/agegate` table tests (parse, lookup, disabled, default).

### Phase 2 — config + wiring — status: open
2. [ ] Add `AgeMinByCountry` env field to `internal/config/config.go` (documented).
3. [ ] Parse in `cmd/app/main.go`, set `authHandler.AgeGate`; `.env.example` + README row.
   - verify: `go build ./...` green.

### Phase 3 — UI + handler — status: open
4. [ ] `web/templ`: `AgeGateView`/`AgeGateCountry` view models + `AgeGateLabel`
   fragment; thread into `RegisterPage`. `age_country`/`age_confirmed` signals
   added to `InitialSignals`. `make gen`.
5. [ ] `auth.Handler`: `AgeGate` field; `countryFromRequest`; `GetRegister`
   builds the view; new `GET /register/age` patches `#age-gate-label` on country
   change; `PostRegister` rejects unticked checkbox when gate enabled.
   - verify: `go build`, gate-off path unchanged; gate-on rejects without checkbox.

### Phase 4 — verify + review — status: open
6. [ ] Codex read-only review of the diff (register = user-input handler).
7. [ ] Playwright/HTTP smoke: gate off (no checkbox), gate on (prefill, change age,
   block without tick, succeed with tick).

## Verification
- `go test ./...` + `go build ./...` green.
- `AGE_MIN_BY_COUNTRY` empty → /register identical to today (no checkbox/select).
- Set → country picker prefilled from CF-IPCountry, age text updates on change,
  Register blocked server-side until the checkbox is ticked.

## Adjustments
- 2606252100 — **Scope cut by user mid-build**: dropped the per-country model
  (dropdown + CF-IPCountry header + ISO country table + `agegate` package +
  consent migration 00077) in favour of ONE global minimum age. Far smaller
  surface. Env var is now `REGISTER_MIN_AGE` (int, 0 = off), not
  `AGE_MIN_BY_COUNTRY`. The deleted `internal/agegate` package and migration are
  gone; number is operator-set (user weighing 14 vs 16; `.env.example` shows 16).

## Progress Log
- 2606252055 — plan created after Step-1 bootstrap + user clarification.
- 2606252100 — scope reduced to single age; agegate pkg + migration removed.
- 2606252105 — shipped: `RegisterMinAge` config, `age_confirmed` signal,
  `AgeGate` templ checkbox + disabled Register, server re-validate in
  PostRegister, main.go wiring, `.env.example`. `make gen` + `go build ./...` +
  `go test ./internal/auth` green. HTTP smoke: checkbox renders, unticked POST
  rejected ("at least 16"), ticked POST proceeds. Committed (feat(auth)).
  Note: gate is honor-based self-attestation; not persisted (no schema change).
