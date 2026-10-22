-- Remove Google Calendar support. Transactional SQL without transaction-control
-- statements: the migration runner wraps it with its tracking row.
DROP TABLE IF EXISTS google_calendar_connections;
