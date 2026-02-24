-- Relationship nicknames: user gives a friend a display nickname.
-- user_id = who set the nickname (the viewer/actor)
-- target_id = the friend being nicknamed

CREATE TABLE IF NOT EXISTS relationship_nicknames (
    user_id bigint,
    target_id bigint,
    nickname text,
    PRIMARY KEY (user_id, target_id)
);
