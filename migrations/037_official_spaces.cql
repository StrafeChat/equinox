-- Official spaces: a space an instance administrator has blessed as part of the instance
-- (e.g. "official Strafe spaces"). Instance-local - a mirror of a space another instance
-- hosts is never marked official here. Toggled from the admin dashboard; the client shows
-- it as a badge beside the space name. NULL (every existing space) reads as false.
ALTER TABLE spaces ADD official boolean;
