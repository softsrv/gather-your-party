-- Page-triggered library sync. Transactional SQL without transaction-control
-- statements: the migration runner wraps it with its tracking row.
-- library_synced_at is the last successful sync (friends refresh and library
-- save both succeeded); a user is due for a sync when it is NULL or older than
-- 24 hours. library_sync_error holds the reason of the last failed sync since
-- then; a successful sync clears it.
ALTER TABLE users ADD COLUMN library_synced_at timestamptz NULL;
ALTER TABLE users ADD COLUMN library_sync_error text NULL;
ALTER TABLE users ADD COLUMN library_sync_error_at timestamptz NULL;
