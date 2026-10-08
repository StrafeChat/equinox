-- Discover across instances: a space listed here can also be shown on the Discover page of
-- every instance that federates with this one.
--
-- Sharing is per space and on by default, so the column records the *exception*: true means
-- the managers asked to be found on this instance only. A listing written before this column
-- existed reads as null, which is the default - shared - and needs no backfill.
ALTER TABLE discover_listings ADD federate_opt_out boolean;

-- What peers told us they list, refreshed on the maintenance timer and read whole when the
-- Discover page is built. One partition: a directory is hundreds of rows, not millions, and
-- a peer's whole set is replaced (or dropped) in one range delete by clustering on domain.
-- space_id is the ORIGIN's id for the space - this instance may not mirror it at all.
CREATE TABLE IF NOT EXISTS discover_remote_listings (
    bucket int,
    domain text,
    space_id bigint,
    name text,
    name_acronym text,
    icon text,
    banner text,
    description text,
    tagline text,
    tags list<text>,
    member_count int,
    online_count int,
    fetched_at timestamp,
    PRIMARY KEY ((bucket), domain, space_id)
);
