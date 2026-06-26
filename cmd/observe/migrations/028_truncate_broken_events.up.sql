-- Clear rows inserted before the propertiesJSON("") -> "{}" fix (2026-06-26).
-- Those rows have empty-string JSONB for the properties column, which causes
-- Nucleus to make them invisible to WHERE-clause queries. They show up in
-- COUNT(*) but stats queries (which all use WHERE site_id = ?) can't see them.
-- TRUNCATE works where DELETE does not on Nucleus with broken JSONB rows.
TRUNCATE events;
TRUNCATE events_recent;
