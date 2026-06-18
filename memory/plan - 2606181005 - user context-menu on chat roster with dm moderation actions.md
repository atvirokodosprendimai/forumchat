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

### Phase 1 - Roster sidebar + menu shell + baseline actions - status: open

> Visible result: right-click (or ⋮) any member, online or offline → menu with Send private message, View profile, Mention, Copy name. Works for every viewer.

1. [ ] `auth.Repo.ListApprovedMembers(ctx, communityID)` → `[]MemberRow{MembershipID, UserID, DisplayName, AvatarURL, Role, BannedUntil}`
   - approved (`approved_at IS NOT NULL`), not rejected/soft-deleted; ordered by DisplayName
   - service/repo test against `t.TempDir()` DB (§11 convention)
2. [ ] View-model + templ: `RosterSidebar` / `RosterList(online []RosterMember, offline []RosterMember, v Viewer)` replacing `#presence-list`
   - `RosterMember` view struct in `web/templ` (no domain import — §4.13); map in handler
   - each `<li>` carries `data-user-id`, `data-membership-id`, `data-role`, online dot; offline group dimmed
   - row: `data-on:contextmenu__prevent="el.dispatchEvent(new CustomEvent('fc:user-menu',{bubbles:true,detail:{id,mid,name,role,online,banned}})); $_ctx_x=evt.clientX; $_ctx_y=evt.clientY"` — set coords in the datastar expr (numbers from `evt`, fine per §4.6)
   - `⋮` button per row dispatches the **same** `fc:user-menu` (touch fallback) using `el.getBoundingClientRect()` for coords
3. [ ] `presence.Handler`: build roster from `auth.Repo.ListApprovedMembers` + overlay Tracker online set; `push()` renders `RosterList` templ (drop raw-string HTML)
   - inject `ListApprovedMembers` via a small local interface in `presence` (no direct service import — §6b anti-pattern)
   - re-render on Tracker change (existing SSE trigger); roster-on-membership-edit refresh is out of scope here
4. [ ] `UserContextMenu(v Viewer)` floating component, mounted once in `chat.templ`
   - consumer wrapper: `data-on:fc:user-menu="$_ctx_user_id=evt.detail.id; $_ctx_membership_id=evt.detail.mid; $_ctx_name=evt.detail.name; $_ctx_role=evt.detail.role; $_ctx_online=evt.detail.online; $_ctx_banned=evt.detail.banned; $_ctx_open=true"`
   - menu `position:fixed`, left/top from `_ctx_x/_ctx_y` (via `data-attr:style` or CSS vars); flip when near viewport edge
   - close: `data-on:click__window="$_ctx_open=false"` (stop-propagation on the menu itself), `data-on:keydown__window="evt.key==='Escape' && ($_ctx_open=false)"`
   - add `_ctx_*` keys to `InitialSignals` so the bag declares them up-front (§4.2)
5. [ ] Baseline menu items (member-level, all viewers)
   - **Send private message** → reuse `$_pm_open_to_user/_pm_open_to_name`, hide for self (`data-show="$_ctx_user_id !== '<myId>'"`)
   - **View profile** → `<a href>` to profile route (verify the route during impl)
   - **Mention** → `$body = ($body? $body+' ':'') + '@' + $_ctx_name + ' '` then focus composer textarea; close menu
   - **Copy name** → `navigator.clipboard.writeText($_ctx_name)`
   - `make gen` after templ edits

### Phase 2 - Moderation: Ban / Unban / Kick (mod + admin) - status: open

> Visible result: a moderator right-clicks a member → Ban (opens cleanup modal), Unban, Kick.

1. [ ] Render mod section only when viewer `isMod` (server-side `if v.Role>=mod`); within it, self-exclude via `data-show`, toggle Ban vs Unban on `$_ctx_banned`
2. [ ] Compact `BanDialog` in `chat.templ` (reuse admin cleanup-signal names `cleanup_chat/threads/posts`, `ban_hours`); Ban item sets `_ctx_membership_id` then opens dialog; submit `@post('/c/{slug}/admin/ban?id=' + $_ctx_membership_id)`
3. [ ] Unban → `@post('/c/{slug}/admin/unban?id=' + $_ctx_membership_id)`; Kick → confirm + `@post('/c/{slug}/admin/remove?id=' + $_ctx_membership_id)` with `cleanup_*`
4. [ ] Verify `/admin/*` routes are role-gated by middleware (these endpoints may currently sit under an admin-only mount; if mod should ban, confirm the gate allows mod or keep ban admin-only). Document the decision in Adjustments.
5. [ ] After ban/kick: roster + chat fat-morph refresh already fire via existing `chat.Bus`/Tracker broadcast — confirm, don't re-implement

### Phase 3 - Role management: Make / Remove moderator (admin only) - status: open

> Visible result: an admin promotes a member to moderator (or demotes) from the menu; role badge updates live.

1. [ ] `auth.Repo.UpdateRole(ctx, membershipID, role)`; admin handler `PostSetRole` (`?id=` + `role` signal, or distinct `/promote` `/demote`)
   - guards (server-side): admin-only, cannot change own role, cannot demote the last admin (`CountAdmins`)
2. [ ] Menu items gated admin + `data-show` on `$_ctx_role` (member → "Make moderator"; moderator → "Remove moderator")
3. [ ] Broadcast roster refresh after change (Tracker push or presence re-render); role badge re-renders
4. [ ] Service test: promote → role==mod; demote-last-admin → rejected

### Phase 4 - Block user (per-viewer mute) - status: open

> Visible result: block someone → their chat bubbles vanish for you only; unblock restores.

1. [ ] Migration: `user_blocks(blocker_id, blocked_id, community_id, created_at, PK(blocker_id,blocked_id,community_id))` (goose, under `internal/storage/sqlite/migrations`)
2. [ ] `auth.Repo` (or new `blocks` repo): `Block / Unblock / ListBlocked(blockerID, communityID) []string`
3. [ ] Chat read model: load viewer's blocked set in `loadRecent`; `MessageView` hides or placeholders blocked authors **per-viewer** (read model is per-viewer already — §6b)
4. [ ] Menu Block/Unblock toggle on `$_ctx_blocked` → `@post('/c/{slug}/block?user=' + $_ctx_user_id)` / `/unblock`; self-refresh via `chat.Bus` broadcast to this viewer
5. [ ] Test: blocked author's message absent from blocker's `loadRecent` view, present for others

### Phase 5 - Report user (to mods) - status: open

> Visible result: report a user with a reason → lands in an admin/mod reports queue.

1. [ ] Migration: `user_reports(id, reporter_id, reported_user_id, community_id, reason, context_ref, status DEFAULT 'open', created_at)`
2. [ ] Repo: insert + `ListOpenReports(communityID)`
3. [ ] Menu Report → small reason modal (real body signal `report_reason`) → `@post('/c/{slug}/report?user=' + $_ctx_user_id)`
4. [ ] Notify mods (`chat.Bus`/NATS ping) + surface open reports in `/admin` (reuse admin templ list pattern)
5. [ ] Test: report row created with status `open`; appears in `ListOpenReports`

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

<!-- timestamped changes as work proceeds -->

## Progress Log

- **2606181005** — Plan created. Scope confirmed via AskUserQuestion: full roster (online+offline), all action groups (DM/Profile/Mention + Ban/Unban/Kick + Make/Remove moderator + Copy/Block/Report), ⋮ touch fallback alongside `data-on:contextmenu`. Code recon captured current presence/DM/ban building blocks and the missing pieces (no membership-id on presence, no role-change endpoint, no contextmenu usage). Mempalace search returned mostly unrelated (vvs) context — nothing reusable for this feature.
