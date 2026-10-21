-- Google Calendar OAuth connections keyed by user.
-- Transactional SQL without transaction-control statements: the migration
-- runner wraps it with its tracking row.
CREATE TABLE google_calendar_connections (
    user_id bigint PRIMARY KEY REFERENCES users(id),
    email text NOT NULL,
    refresh_token text NOT NULL DEFAULT '',
    access_token text NOT NULL DEFAULT '',
    token_expiry timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
