BEGIN;

CREATE TABLE users (
    id bigserial PRIMARY KEY,
    steam_id_64 text NOT NULL UNIQUE,
    persona_name text NOT NULL,
    avatar_small text NOT NULL,
    avatar_medium text NOT NULL,
    avatar_full text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token text PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users(id),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

COMMIT;
