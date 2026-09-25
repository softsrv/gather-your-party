BEGIN;

CREATE TABLE parties (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    leader_id bigint NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    party_id uuid NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users(id),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (party_id, user_id)
);

CREATE TABLE invites (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    party_id uuid NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users(id),
    inviter_id bigint NOT NULL REFERENCES users(id),
    status text NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX invites_pending_party_user_idx ON invites (party_id, user_id) WHERE status = 'pending';

CREATE TABLE rejection_tallies (
    party_id uuid NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users(id),
    count integer NOT NULL DEFAULT 0,
    PRIMARY KEY (party_id, user_id)
);

COMMIT;
