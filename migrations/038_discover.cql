-- Discover: the instance's directory of spaces and bots. A space's manager or a bot's
-- owner applies with a tagline and tags, an instance administrator approves or declines,
-- and approved listings are what the Discover page shows every signed-in user. One row
-- per listed thing; a partition per kind, read whole (an instance lists hundreds, not
-- millions). Declining keeps the row with the administrator's note so the applicant can
-- read why and apply again.
CREATE TABLE IF NOT EXISTS discover_listings (
    kind text,
    id bigint,
    status text,
    tagline text,
    tags list<text>,
    requested_by bigint,
    requested_at timestamp,
    reviewed_by bigint,
    reviewed_at timestamp,
    note text,
    PRIMARY KEY ((kind), id)
);
