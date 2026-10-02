-- Durable relay outbox: every server-to-server relay (messages, rooms, relationships,
-- space changes) is written here before it is sent and deleted once the peer has taken
-- it, so a peer that is down gets everything it missed, in order, when it comes back.
-- Rows carry a TTL (see federation/outbox.go); an entry nobody could deliver in that
-- time is dropped.
CREATE TABLE IF NOT EXISTS federation_outbox (
    domain text,
    seq bigint,
    method text,
    path text,
    body text,
    created_at timestamp,
    PRIMARY KEY (domain, seq)
) WITH CLUSTERING ORDER BY (seq ASC);

-- A peer the outbox cannot reach is marked, so operators can see a degraded peer in
-- GET /federation/peers; cleared on the next successful relay.
ALTER TABLE federation_peers ADD last_error text;
ALTER TABLE federation_peers ADD last_failure timestamp;
