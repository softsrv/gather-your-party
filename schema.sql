-- Schema for Gather Your Party
-- Apply once before first use: psql "$DATABASE_URL" -f schema.sql

CREATE TABLE IF NOT EXISTS users (
    id          bigserial PRIMARY KEY,
    steamid64   text        NOT NULL UNIQUE,
    persona     text        NOT NULL DEFAULT '',
    avatar_url  text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    token       text        PRIMARY KEY,
    user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
