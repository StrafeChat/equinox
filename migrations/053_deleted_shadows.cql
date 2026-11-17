-- Account deletion (instance administrators, internal/modules/instance/account_delete.go).
--
-- A peer's shadow of a remote account that its home instance deleted keeps its row - so the
-- messages, reactions and pins that name it still resolve, and a later reference to the same
-- federated id never re-fetches a profile - but loses every profile field; deleted_at marks
-- it. Local accounts are removed outright (users row plus their users_by_username and
-- users_by_email claims) and never carry this column. Additive.
--
-- (052 is taken by the Go data migration 052_embed_links_permission in spaces/backfill.go.)
ALTER TABLE users ADD deleted_at timestamp;
