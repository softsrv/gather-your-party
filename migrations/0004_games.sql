-- Steam game libraries. Transactional SQL without transaction-control
-- statements: the migration runner wraps it with its tracking row.
-- Every user that exists before this migration signed in; rows created later
-- (for example friends recorded by the login sync) start as never logged in.
ALTER TABLE users ADD COLUMN ever_logged_in boolean NOT NULL DEFAULT true;
ALTER TABLE users ALTER COLUMN ever_logged_in SET DEFAULT false;

ALTER TABLE users ADD COLUMN game_sync_state text NOT NULL DEFAULT 'never_synced'
    CHECK (game_sync_state IN ('never_synced', 'synced', 'private_or_no_games'));
ALTER TABLE users ADD COLUMN game_sync_state_at timestamptz NULL;

CREATE TABLE games (
    id bigserial PRIMARY KEY,
    app_id integer NOT NULL,
    name text NOT NULL DEFAULT '',
    img_icon_url text NOT NULL DEFAULT '',
    details_fetched_at timestamptz NULL,
    has_data boolean NOT NULL DEFAULT false,
    multiplayer boolean NOT NULL DEFAULT false
);

CREATE UNIQUE INDEX games_app_id_key ON games (app_id);

CREATE TABLE user_games (
    user_id bigint NOT NULL REFERENCES users(id),
    game_id bigint NOT NULL REFERENCES games(id),
    position integer NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, game_id)
);
