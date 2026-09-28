# ⚠️ Warning:

**Strafe `Equinox` (Backend) and all other projects related to this such as `web.strafe.chat, Nebula (FileSystem), Stargate, etc.` are still under heavy development. We reccommend that you not try to use it for personal use until development has progressed more and bugs are less likely to happen.**

## Equinox
Equinox is the code name for our main backend service made using Golang and Fiber.

# ⚠️ Equinox is currently being rewritten from the previous AI slop.

## Database migrations

Schema changes live in `migrations/*.cql` and are applied, in file order, by the
`migrate` command. It records what it ran in a `schema_migrations` table so it is safe
to run repeatedly. A keyspace that already has tables but no `schema_migrations` (one
migrated by hand with `cqlsh`) is refused: some files are destructive (`019` drops and
recreates `device_keys`), so replaying them would wipe data. Adopt such a keyspace once
with `-adopt`, which records every current file as applied without executing anything.

```bash
go run ./cmd/migrate -dry-run          # list pending migrations
go run ./cmd/migrate                   # apply them to SCYLLA_KEYSPACE from .env
go run ./cmd/migrate -adopt            # hand-migrated keyspace: record files as applied, once
go run ./cmd/migrate -create-keyspace  # also create the keyspace on a fresh cluster
ENV_FILE=.env.other go run ./cmd/migrate
```

The Docker deployment in `deploy/` runs the same command before starting the API.
