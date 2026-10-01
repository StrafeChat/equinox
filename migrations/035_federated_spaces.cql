-- Spaces that span instances.
--
-- A space is hosted by the instance that created it (its origin). Every other instance
-- with a member keeps a mirror: its own spaces row, rooms, roles, overrides and member
-- rows under local ids, tied to the origin's identity through federated_spaces (and the
-- existing federated_rooms tables for each channel). federated_space_peers is kept by the
-- origin: which instances mirror the space, so a change is relayed to each once.
CREATE TABLE IF NOT EXISTS federated_spaces (
    space_id bigint,
    origin_domain text,
    origin_space_id bigint,
    created_at timestamp,
    PRIMARY KEY (space_id)
);

CREATE TABLE IF NOT EXISTS federated_spaces_by_origin (
    origin_domain text,
    origin_space_id bigint,
    space_id bigint,
    PRIMARY KEY (origin_domain, origin_space_id)
);

CREATE TABLE IF NOT EXISTS federated_space_peers (
    space_id bigint,
    domain text,
    created_at timestamp,
    PRIMARY KEY (space_id, domain)
);
