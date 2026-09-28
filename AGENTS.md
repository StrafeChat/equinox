# equinox — AGENTS.md

`equinox` is StrafeChat's Go/Fiber backend: the REST API plus `stargate`, the realtime
WebSocket gateway (its own binary under `cmd/stargate`, sharing ScyllaDB and Redis with
the API). If you only have this repo checked out, read this whole file before making
changes — it's self-contained. The full project (this repo, the web client, object
storage, deployment) is normally developed side by side; if you have that workspace too,
its root `AGENTS.md` has the complete picture.

## Mission — why this backend exists

StrafeChat is a Discord-alternative built around: **privacy** (minimum data collection, no
trackers, no silent telemetry), **real end-to-end encryption** (an actual Double
Ratchet/Megolm, not a static-key stand-in — the E2EE layer is mid-overhaul toward
vodozemac; see `internal/modules/devices`), **federation** (independent instances talk
over signed HTTP, see [`docs/FEDERATION.md`](https://github.com/StrafeChat/deploy/blob/main/docs/FEDERATION.md)), **self-hosting** (env-var configuration,
`docker-compose.yml`, `deploy/`), and **client customization**. Don't trade any of these
away for convenience — flag the conflict instead.

## Terminology

We reuse Discord's interaction model, never its names. Use these everywhere — schema,
Go identifiers, error strings, docs:

| Discord term | Our term |
|---|---|
| Server / Guild | **Space** (`spaces` module, `SpaceID`) |
| Channel | **Room** (`rooms` module; a space's rooms are `space_rooms`) |
| Category | **Section** — not built yet; use this name when it is |
| DM | **PM** (`RoomType` 1 = PM, 2 = group PM) |
| AFK channel | AFK room in code is fine; user-facing copy says "inactive voice channel" |
| Server Widget | New copy says "space widget"; existing `widget_*` fields can stay as-is |

## Architecture conventions — follow these, don't reinvent them

- **Module layout**: `internal/modules/<domain>/{model.go, repo.go, service.go,
  handler.go}`. A domain that outgrows one file splits by *feature*, not by layer:
  `service_invites.go`, `handler_invites.go`, `service_emoji.go`, etc., all extending the
  same `Service`/`Handler` struct declared in the base file. Mirror this when a module
  grows (see `spaces/` and `messages/` for the pattern).
- **Migrations**: `migrations/NNN_description.cql`, zero-padded 3-digit prefix, applied
  in lexicographic filename order by `cmd/migrate` (which tracks what it's run in
  `schema_migrations`). Additive-only by default — a destructive migration needs a loud
  comment explaining why. **Never replay an already-applied migration against a live
  keyspace**; `019` drops and recreates `device_keys`, and replaying it once already
  destroyed real E2EE key material. `cmd/migrate` refuses an untracked keyspace unless
  given `-adopt` — never bypass that check.
- **Permissions**: the full Discord-style bitmask lives in
  `internal/modules/permissions/bits.go`. Check with `permissions.Has(perms,
  permissions.PermX)`; resolve effective permissions through each service's own
  `authorize()`/`EffectiveChannelPermissions` helper rather than hand-rolling a check.
  Some bits (e.g. `PermAddReactions`, `PermUseExternalEmojis`) are pre-declared ahead of
  the feature that enforces them — check `bits.go` before assuming a permission doesn't
  exist yet.
- **Audit log**: every moderator-facing mutation (kick, ban, role/room/emoji/invite
  changes) calls `s.audit(ctx, spaceID, actorID, ActionType, targetID, changes, reason)`
  (`internal/modules/spaces/audit.go`) — best-effort, logs on failure, never blocks the
  request. High-frequency, non-moderation actions (reactions, typing, read receipts)
  deliberately skip this.
- **Realtime events**: `internal/stargate.PublishToSpace(ctx, redis, roomID, "EVENT_TYPE",
  data, region)` / `PublishToUser(...)` publish to Redis, which every Stargate instance
  fans out to subscribed WebSocket clients as an `op:3 EVENT` frame. Event types are
  `SCREAMING_SNAKE_CASE` verbs (`SPACE_UPDATE`, `MESSAGE_REACTION_ADD`). `docs/STARGATE.md`
  documents the READY payload shape and every event type — keep it updated when you add
  one.
- **Error handling**: define sentinel errors per service (`ErrXNotFound`, `ErrForbidden`,
  ...) and map them to HTTP status in one central switch per module (see `spaceError` in
  `spaces/handler.go`, `reactionError` in `messages/handler_reactions.go`) instead of
  hand-rolling the mapping in every handler.
- **Federation hooks**: services that touch cross-instance state implement a small
  `Federator` interface (`AfterMessageCreated`, etc.) — calls are best-effort and must
  never block or fail the local request.
- **Fiber gotcha**: `UnescapePath` is off (the default) — a route param that can contain
  non-ASCII (e.g. a unicode emoji) arrives at `c.Params()` still percent-encoded. Decode
  it explicitly with `url.PathUnescape` in the handler; don't flip the app-wide config.
- **Background goroutines go through `internal/safego`** (`safego.Go` for fire-and-forget,
  `safego.Run` inside a WaitGroup worker). Fiber's recover middleware only covers the
  request goroutine; a bare `go func()` that panics takes the whole API process down.
- **Auth is the `Authorization: Bearer` header only.** The REST middleware deliberately
  accepts no cookie and no `?token=` query parameter. The WebSocket gateway is the one
  exception (browsers cannot set headers on a WebSocket), which is why
  `STARGATE_ALLOWED_ORIGINS` is required whenever federation is on — without an origin
  check, any site could open an authenticated gateway connection in a signed-in user's name.
- **Voice/video (`internal/modules/voice`)**: LiveKit carries the media; this module is
  the authority on who is in which voice room (Redis-backed `voice.State`, one room per
  user), what they may publish (the join token's sources and `UpdateParticipant` follow
  the space's voice permission bits and moderator mute/deafen - LiveKit enforces them,
  the client only mirrors them), PM call ringing, and cleanup (LiveKit webhooks plus a
  30 s reconciler). Moderator actions go through `spaces.Service.RecordAudit`. New
  voice behaviour belongs here, gated by a permission bit and enforced at LiveKit where
  it can be, not in the client. [`docs/VOICE.md`](https://github.com/StrafeChat/deploy/blob/main/docs/VOICE.md) has the map.
- **One-shot data fixes** (`spaces/backfill.go`): claim a name in `data_migrations` with
  `ClaimDataMigration` (a lightweight transaction), do the work, release the claim on
  failure. CQL migrations cannot express "OR a bit into every existing row"; this can.

## Verification expectations

Before calling backend work done: `go build ./...`, `go vet ./...`, `go test ./...`, and
for anything with real request/response behavior, an actual smoke test (curl against the
running dev server, or the equivalent) — not just "it compiles."
