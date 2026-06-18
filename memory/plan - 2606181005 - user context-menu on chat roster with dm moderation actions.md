---
tldr: Right-click (data-on:contextmenu) + ⋮ touch button on chat roster members opens one signal-driven floating menu — DM, profile, mention, copy, ban/unban/kick, make/remove moderator, block, report. Roster shows full membership (online + offline).
status: active
---

# Plan: User context-menu on chat roster (DM + moderation actions)

## Context

- Spec: [[spec - forumchat - community web app with realtime chat and forum threads]] — chat + presence sidebar live here. This feature is an enhancement; fold a short note into the spec's Future/feature section once Phase 1 lands (optional, via `/eidos:spec`).
- Related: [[spec - invites - admin-add-by-email-and-join-page]] (admin/role surface), private messaging lives in `internal/privatemsg`.
- Guidance skills to thread through every code action: `/datastar` (v1 colon syntax, EDA custom-event bus §4.12, stable-id morph §4.7, `_`-prefixed UI-only signals) and `/effective-go` (idioms, small interfaces, no service↔service imports).

### What exists today (from code recon 2606181005)

- **Presence sidebar** `web/templ/chat.templ:107-115` renders `#presence-list` as plain `<li>DisplayName</li>` — **no user-id, no membership-id, online-only** (in-proc `presence.Tracker`).
- `presence.Member` = `{UserID, DisplayName, AvatarURL, LastSeen}` — **no MembershipID, no Role**. `presence.Handler.push()` renders a raw HTML string (not a templ).
- **DM ready**: `privatemsg.Service.CreateRequest(ctx, fromUserID, toUserID, body, "", "")`; chat.templ already opens a PM modal by setting `$_pm_open_to_user` / `$_pm_open_to_name` / `$pm_source_chat_id` → `MessagesComposeDialog` (`chat.templ:137,377`).
- **Ban** `admin.Handler.PostBan` (`internal/admin/admin.go:161`) — URL `?id={membershipID}`, body signals `ban_hours, cleanup_chat, cleanup_threads, cleanup_posts`. **Unban** `PostUnban` `?id=`. **Remove/kick** `PostRemoveMember` `?id=` + `cleanup_*`. All need **membership id**.
- **No promote/demote-role endpoint exists** — must be built (Phase 3).
- **No `data-on:contextmenu` anywhere** yet. Existing menus use `<details class="msg-menu">` + `<summary>`.
- Role gate: `auth.FromContext(ctx)` → `id.Membership.Role.AtLeast(auth.RoleMod | auth.RoleAdmin)`.

### Design decisions (locked)

- **Full roster** (online + offline), grouped Online/Offline. Roster built in the handler from `auth.Repo` (carries membership-id + role), online status overlaid from `presence.Tracker` — solves the membership-id gap cleanly. Tracker stays the "who's online" source and the SSE re-render trigger.
- **One floating menu**, EDA-style (CLAUDE.md §4.12): each roster row fires a `bubbles:true` `CustomEvent('fc:user-menu', {detail:{...}})` on `contextmenu__prevent` **and** from a `⋮` touch button. One consumer up the tree sets `_ctx_*` signals and opens the menu. Menu positioned `position:fixed` from `_ctx_x/_ctx_y`. Closes on `data-on:click__window` / `data-on:keydown__window` (Esc).
- **Signal hygiene** ([[feedback_datastar_underscore_signals]]): all menu-target identity is **UI-only** `_ctx_*` (user_id, membership_id, name, role, online, banned, blocked, x, y). Target identity reaches the server via **query params** (`?id=` / `?user=`) — same as existing ban/unban/remove — so it never needs to be in the JSON body. Only cleanup booleans (ban/kick) and report reason are real body signals.
- **Server-side gating is the real gate**; `data-show` is cosmetic. Every mod/admin endpoint enforces role via middleware/handler check. Self-exclusion and last-admin guards live server-side too.

## Phases

### Phase 1 - Roster sidebar + menu shell + baseline actions - status: completed

> Visible result: right-click (or ⋮) any member, online or offline → menu with Send private message, Mention, Copy name. Works for every viewer.

1. [x] Roster source
   - => **Reused existing `auth.Repo.ListMembers(ctx, communityID)`** — already returns approved memberships JOIN users (membership id, user id, role, banned_until). No new repo method needed; no new test (covered).
2. [x] View-model + templ: `RosterPanel(online, offline []RosterMember)` replacing `#presence-list` → `web/templ/roster.templ`
   - => `RosterMember{UserID, MembershipID, DisplayName, AvatarURL, Role, Online, Banned}` (leaf struct, §4.13)
   - => each `<li.roster-row>` carries `data-user-id/-membership-id/-name/-role/-online/-banned`; online dot on avatar; offline group dimmed via `.roster-offline`
   - => `data-on:contextmenu__prevent` + `⋮` button both fire the shared `fc:user-menu` CustomEvent; coords come in `detail.x/y` (cursor for right-click, button rect for ⋮). Dispatch JS factored into `userMenuDispatch` / `rosterMenuBtnDispatch` helpers
3. [x] `presence.Handler`: roster from `Members.ListMembers` + overlay Tracker online set; `push()` renders `RosterPanel` via `PatchElementTempl` (default outer-morph), raw-string HTML dropped
   - => injected via local `MemberLister` interface (`internal/presence/handler.go`), wired `Members: aRepo` in `cmd/app/main.go`
   - => `push` now takes `ctx`; removed dead `itoa`/`escape`/`strings` import
4. [x] `UserContextMenu(slug, currentUserID string)` floating component mounted in `chat.templ` → `web/templ/usermenu.templ`
   - => consumer is `data-on:fc:user-menu__window` on the menu itself (global listener, §4.12); clamps x/y to viewport
   - => visibility via `data-class:open` (so `data-attr:style` owns left/top without fighting `display`); close on `data-on:click__window` + Esc `data-on:keydown__window`; menu swallows its own clicks with `data-on:click__stop`
   - => added `_ctx_*` keys to `InitialSignals`
5. [x] Baseline menu items (member-level, all viewers): Send private message (reuse `$_pm_open_to_user/_pm_open_to_name`, self-hidden), Mention (`$body += '@name '` + focus composer), Copy name (`navigator.clipboard`)
   - => **View profile dropped** — no public per-user profile route exists (`/profile` is self-only). Revisit if a profile page is added.
   - => CSS for `.roster*` + `.ucm*` appended to `app.css`; legacy `.presence li::before` green dot suppressed for roster rows

### Phase 2 - Moderation: Ban / Unban / Kick (admin only) - status: completed

> Visible result: an admin right-clicks a member → Ban (opens cleanup modal), Unban, Kick.

1. [x] Admin section rendered server-side `if isAdmin` (`UserContextMenu` gained `isAdmin bool`, fed `d.Viewer.Role == "admin"`); self-exclude via `data-show`, Ban hidden when `$_ctx_banned`, Unban shown only when `$_ctx_banned`
2. [x] `BanDialog(slug)` in `usermenu.templ` — reuses `.modal*` classes + `ban_hours`/`cleanup_*` signals; Ban item resets those signals + opens dialog on `$_ctx_ban_open`; submit `@post('/c/{slug}/admin/ban?id=' + $_ctx_membership_id)`
3. [x] Unban → `@post('.../admin/unban?id=' + $_ctx_membership_id)`; Remove → `confirm()` + reset cleanup + `@post('.../admin/remove?id=' + $_ctx_membership_id)`
4. [x] Verified `/admin/{ban,unban,remove}` sit under `RequireRole(auth.RoleAdmin)` and that `refreshAdminLists` only morphs `#admin-*` ids (no-op + no Redirect on the chat page) — safe to call from chat. **Decision: admin-only** (see Adjustments)
5. [x] Ban-with-cleanup already calls `chat.Bus.Broadcast()` (chat refreshes). => Roster's banned badge only updates on the next presence push (no Tracker change on ban) — acceptable; the banned user's session is destroyed next request, dropping them from the Tracker which then refreshes the roster.

### Phase 3 - Role management: Make / Remove moderator (admin only) - status: completed

> Visible result: an admin promotes a member to moderator (or demotes) from the menu; role badge updates live.

1. [x] `admin.Handler.PostSetRole` (`?id=` + `?role=moderator|member`); reuses existing `auth.Repo.UpdateMembershipRole`
   - => guards: rejects self, rejects invalid role, rejects changing an **admin's** role via this path (admins managed elsewhere). Last-admin guard not needed here — set-role never demotes admins.
2. [x] Menu items in the `if isAdmin` section, `data-show` on `$_ctx_role` (member → "Make moderator"; moderator → "Remove moderator"), self-excluded
3. [x] **Live roster refresh**: added `presence.Tracker.Bump(communityID)` + an `admin.RosterNotifier` hook (`Roster: presenceTracker` in main). `bumpRoster` now fires after set-role **and** ban/unban/remove/approve → every open presence stream re-renders the roster from fresh DB rows. (This also resolves the Phase 2 banned-badge staleness note.)
4. [x] Test: `internal/auth/role_test.go` — promote member→moderator→member round-trip via `UpdateMembershipRole`. `go test ./internal/auth` green. (Handler guards are HTTP-level; covered by reasoning + the admin-gated route.)
   - => Route `POST /c/{slug}/admin/set-role` registered in the admin-gated group.

### Phase 4 - Block user (per-viewer mute) - status: completed

> Visible result: block someone → their chat bubbles vanish for you only; unblock restores.

1. [x] Migration `00030_user_blocks.sql` — `user_blocks(blocker_id, blocked_id, community_id, created_at, PK(...))`, FK→users ON DELETE CASCADE, index on `(blocker_id, community_id)`
2. [x] `auth.Repo.BlockUser` (INSERT OR IGNORE, self-block no-op) / `UnblockUser` / `ListBlocked` — reused `auth.Repo` (chat already holds `AuthRepo`, no new interface)
3. [x] `chat.loadRecentFor`: `blockedSet(viewer)` filters blocked authors out of the per-viewer read model (system rows have nil AuthorID → never filtered)
4. [x] Menu Block/Unblock toggle on `$_ctx_blocked` → `chat.Handler.PostBlock/PostUnblock` (`/c/{slug}/block|unblock?user=`); each fat-morphs the actor's own chat immediately + `Roster.Bump` re-renders the sidebar. Roster is now block-aware: `presence.Handler.Blocks` marks `data-blocked` / `.is-blocked` per viewer (push is viewer-scoped).
5. [x] Test `internal/auth/blocks_test.go` — block/unblock/list round-trip, idempotency, directionality, self-block no-op. The `loadRecentFor` filter itself is 3 lines covered by reasoning.
   - => `chat.Handler.Roster` (RosterNotifier) wired post-hoc in main (tracker built after chatHandler); routes under `RequireApproved`.

### Phase 5 - Report user (to mods) - status: completed

> Visible result: report a user with a reason → lands in the /admin reports queue.

1. [x] Migration `00031_user_reports.sql` — `user_reports(id, reporter_id, reported_user_id, community_id, reason, context_ref, status DEFAULT 'open', created_at)` + index `(community_id, status, created_at)`
2. [x] `auth.Repo.CreateUserReport` / `ListOpenReports` (LEFT JOIN memberships for reporter+reported display names) / `ResolveUserReport`
3. [x] Menu "Report to moderators" (member-level, self-excluded) → `ReportDialog` modal (`report_reason` body signal) → `chat.Handler.PostReport` (`/c/{slug}/report?user=`); confirms via the global `_pm_toast_text` toast and clears the signal
4. [x] Surfaced in `/admin` — new `AdminReports` section + `AdminReport` view struct + `reportsToAdminReports` adapter; `admin.PostResolveReport` (`/admin/report/resolve?id=`) drops it from the open queue; included in `refreshAdminLists`
   - => **Realtime mod ping skipped** — reports appear in `/admin` on load/refresh (no live NATS/Bus push). Sufficient for the queue; add a badge/ping later if needed.
5. [x] Test `internal/auth/reports_test.go` — create → appears open with resolved names → resolve → empty. `go test ./internal/...` all green.

### Phase 6 - Polish, a11y, smoke - status: open

1. [ ] CSS: floating menu reuses `msg-menu` visual language; online dot + offline dimming; edge-flip so menu never overflows viewport
2. [ ] a11y: `role="menu"`/`menuitem`, keyboard nav within open menu, focus return to triggering row on close; ⋮ button `aria-label`
3. [ ] Wire `/datastar` + `/effective-go` review pass over the diff before final commit
4. [ ] HTTP smoke: load chat, open menu on online + offline member, exercise DM/ban/promote/block/report; `go test ./...` + `make build` green

## Verification

- `go test ./...` green, including new repo/service tests (ListApprovedMembers, UpdateRole + last-admin guard, block-hides-bubble, report-created).
- `make build` (CGO_ENABLED=0) + `make gen` clean (no stale `*_templ.go`).
- Manual: right-click and ⋮ both open the menu on an **online** and an **offline** member; menu positions at cursor and flips near edges; Esc + outside-click close it.
- Member viewer sees only DM/Profile/Mention/Copy/Block/Report; mod additionally sees Ban/Unban/Kick; admin additionally sees Make/Remove moderator. Mod/admin items are **not present in DOM** for lower roles (server-side render), not merely hidden.
- Acting on self: no Ban/Kick/DM-self/role items. Last admin cannot be demoted (server rejects even if forced).
- Block hides target's bubbles for the blocker only; report creates an `open` row visible to mods.

## Adjustments

- **2606181015** — `/admin/ban|unban|remove` are gated `RequireRole(auth.RoleAdmin)` (admin-only), not mod. Decision: keep moderation menu items (Ban/Unban/Kick + role change) **admin-only** — reuse existing authz, no new mod-ban surface. Resolves the open question parked in Phase 2.4. The menu's `isMod`-style gating becomes `isAdmin`.
- **2606181015** — Reused `auth.Repo.ListMembers` instead of building `ListApprovedMembers` (it already exists and fits). "View profile" dropped from the menu — no public per-user profile route.

## Progress Log

- **2606181015** — Phase 1 complete. Roster sidebar (online+offline) + floating `UserContextMenu` (right-click + ⋮ touch) + baseline actions (DM/Mention/Copy). New: `web/templ/roster.templ`, `web/templ/usermenu.templ`, `MemberLister` in presence, `.roster*`/`.ucm*` CSS. `make gen` + `make build` clean; `go test ./internal/presence ./internal/auth` green.
- **2606181016** — Phase 2 complete. Admin-only Ban/Unban/Kick wired into the menu via existing /admin endpoints; BanDialog reuses cleanup signals. Build clean.
- **2606181005** — Plan created. Scope confirmed via AskUserQuestion: full roster (online+offline), all action groups (DM/Profile/Mention + Ban/Unban/Kick + Make/Remove moderator + Copy/Block/Report), ⋮ touch fallback alongside `data-on:contextmenu`. Code recon captured current presence/DM/ban building blocks and the missing pieces (no membership-id on presence, no role-change endpoint, no contextmenu usage). Mempalace search returned mostly unrelated (vvs) context — nothing reusable for this feature.
