-- +goose Up

-- Server-owned resume cursor per connector, so an external worker is "almost
-- stateless": after a disconnect it just reconnects with its id+secret and the
-- server replays the messages it missed from where delivery last stopped — no
-- client-side watermark bookkeeping required (spec-connectors "Backlog replay").
--
-- cursor_at is the unix-second watermark of the furthest message delivered to
-- this connector's stream. It is written ONCE on stream close, never per message
-- — a per-message UPDATE would be a write-on-read storm against the single-writer
-- SQLite handle (AGENTS §8) and contend with real chat writes.
--
-- Semantics of the value:
--   NULL  → no position yet; the first connect is live-only (nothing to replay).
--   0     → an admin pressed "Reset replay"; the next connect replays the whole
--           catch-up window (bounded server-side, see internal/connectors).
--   >0    → resume delivery from that instant on the next connect.
-- An explicit ?since=<unix> or ?live=1 on the stream URL overrides the cursor.
ALTER TABLE connectors ADD COLUMN cursor_at INTEGER;

-- +goose Down
ALTER TABLE connectors DROP COLUMN cursor_at;
