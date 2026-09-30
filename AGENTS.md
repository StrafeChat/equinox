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
- **Auth is the `Authorization` header only** — no cookie, no `?token=` query parameter.
  The header carries one of three credentials, all resolved by `middleware.RequireAuth`:
  a session `Bearer` token; an OAuth2 `Bearer` access token (tried when the token is not a
  live session, its granted scopes recorded in `Locals[middleware.LocalsKeyScopes]` and
  gating scope-limited fields such as the account email in `/users/@me`); or a
  `Bot <token>` bot-account token. The `applications`/`oauth` modules inject their
  resolvers at startup with `middleware.SetBotResolver`/`SetOAuthResolver`, so the
  middleware never imports them (they import `auth`, which would cycle). The WebSocket
  gateway is the one exception (browsers cannot set headers on a WebSocket), which is why
  `STARGATE_ALLOWED_ORIGINS` is required whenever federation is on — without an origin
  check, any site could open an authenticated gateway connection in a signed-in user's name.
- **Developer platform (`internal/modules/applications` + `internal/modules/oauth`)**:
  OAuth2 applications and bot accounts, modelled on Discord. An application owns redirect
  URIs, a hashed client secret, a `bot_public` switch and optionally a bot user (an
  `auth.User` with `Bot` set, a synthetic `bot+<id>@bots.invalid` email so it never
  collides on the empty-email login key, and **the application's id as its user id** so an
  install link needs no lookup). Client secrets and bot tokens are hashed at rest with the
  session-token scheme (`hex(sha256(hex-decoded raw))`) and shown exactly once at
  mint/reset; a bot token reset publishes `SESSION_REVOKED` so the gateway drops the bot.
  `oauth` is the authorization server: authorization-code grant (HTTP Basic or body client
  auth, form or JSON), scopes `identify` / `email` / `spaces` / `spaces.join` / `bot`,
  single-use codes, one live access+refresh pair per (user, app) - refresh and
  re-authorisation replace it, so a grant revoke is total - RFC 7009 revoke, and
  `GET /oauth2/@me`. **Scopes are enforced by the middleware**: `RequireAuth` refuses OAuth2
  tokens with `403 insufficient_scope`; only routes wrapped in `RequireAuthScoped(...)`
  (`GET /users/@me`, `GET /users/@me/spaces`, `GET /oauth2/@me`) admit them, so a new
  endpoint is closed to third-party apps unless it opts in. The `bot` scope installs the
  bot into a space the consenting user manages (`spaces.Service.InstallBot`: owner,
  Administrator or Manage Space; permissions masked to what they may grant) with a
  **managed role** (`space_roles.bot_id`, position 1, undeletable, unassignable, removed
  with the bot); `spaces.join` lets a bot with Create Invite add an authorising user
  (`PUT /spaces/:id/members/:user_id`). Bots use the gateway with `Authorization: Bot`
  on the handshake (header only) and are auto-subscribed to everything in READY
  (`ReadyData.ChannelIDs`). The developer documentation for all of this is the docs site
  in `web.strafe.chat/docs/pages/` (served at `/docs/` on every instance) - update it in
  the same change when an endpoint, scope, event or permission changes.
- **Profile badges** are a `public_flags` bitfield on the user
  (`internal/modules/auth/badges.go`; append-only bits behind an `AllBadges` mask). An
  instance admin assigns them through moderation (`instance.SetUserBadges`), and every user
  payload exposes `auth.PublicFlags(u)` — the `Flags` column masked to known bits — never
  the raw column.
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
