-- Party events scheduled by the party leader. Transactional SQL without
-- transaction-control statements: the migration runner wraps it with its
-- tracking row. start_at and end_at are absolute instants (timestamptz); the
-- browser's local date, times and UTC offset are converted before saving.
CREATE TABLE party_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    party_id uuid NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    game_id bigint NOT NULL REFERENCES games(id),
    start_at timestamptz NOT NULL,
    end_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (end_at > start_at)
);

CREATE INDEX party_events_party_id_end_at_idx ON party_events (party_id, end_at);
