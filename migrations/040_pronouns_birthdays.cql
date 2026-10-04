-- Profile pronouns (free text, public like the bio) and opt-in birthdays.
ALTER TABLE users ADD pronouns text;
-- Birthdays are opt-in for privacy: only when true do the birthday fields appear on a profile
-- and does the user get announced in spaces.
ALTER TABLE users ADD birthday_opt_in boolean;

-- Who has a birthday on a given month/day, so the daily announcement job is one partition read
-- per day instead of a full user scan. Maintained as users opt in/out (and only for users with
-- a date of birth set).
CREATE TABLE IF NOT EXISTS birthdays_by_day (
  month int,
  day int,
  user_id bigint,
  PRIMARY KEY ((month, day), user_id)
);

-- A space can post a birthday message for opted-in members in this channel; birthday_message is
-- an optional custom template (empty = the client's default greeting).
ALTER TABLE spaces ADD birthday_channel_id bigint;
ALTER TABLE spaces ADD birthday_message text;
