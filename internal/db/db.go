// Package db provides persistence for users, sessions, and parties.
package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// Store uses a pool owned and closed by the application.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// UpsertUser persists a profile for a caller-verified SteamID64. The caller must
// verify ownership before calling; Player.SteamID is never used as identity.
func (s *Store) UpsertUser(ctx context.Context, verifiedSteamID64 string, player steamapi.Player) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (steam_id_64) DO UPDATE SET
			persona_name = EXCLUDED.persona_name,
			avatar_small = EXCLUDED.avatar_small,
			avatar_medium = EXCLUDED.avatar_medium,
			avatar_full = EXCLUDED.avatar_full,
			updated_at = now()
		RETURNING id`, verifiedSteamID64, player.PersonaName, player.AvatarSmall, player.AvatarMedium, player.AvatarFull).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert user: %w", err)
	}
	return id, nil
}

// UserProfile returns the stored Steam profile for a verified SteamID64. UpsertUser
// refreshes it from Steam at every sign-in, so pages can show the user's name and
// avatar without calling Steam on each request.
func (s *Store) UserProfile(ctx context.Context, steamID64 string) (steamapi.Player, bool, error) {
	player := steamapi.Player{SteamID: steamID64}
	err := s.pool.QueryRow(ctx, `
		SELECT persona_name, avatar_small, avatar_medium, avatar_full FROM users WHERE steam_id_64 = $1`, steamID64).
		Scan(&player.PersonaName, &player.AvatarSmall, &player.AvatarMedium, &player.AvatarFull)
	if errors.Is(err, pgx.ErrNoRows) {
		return steamapi.Player{}, false, nil
	}
	if err != nil {
		return steamapi.Player{}, false, fmt.Errorf("user profile: %w", err)
	}
	return player, true, nil
}

// ResolveUserID turns a verified SteamID64 into its users.id. Because
// steam_id_64 is UNIQUE, a known SteamID64 yields exactly one id; an unknown
// one returns pgx.ErrNoRows-derived not-found (mirroring ResolveSession).
func (s *Store) ResolveUserID(ctx context.Context, steamID64 string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM users WHERE steam_id_64 = $1`, steamID64).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve user id: %w", err)
	}
	return id, true, nil
}

// CreateSession creates a seven-day session with a 256-bit random opaque token.
// A token is returned only after its database row has been stored successfully.
func (s *Store) CreateSession(ctx context.Context, userID int64) (string, error) {
	token, err := newSessionToken()
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO sessions (token, user_id, expires_at, created_at)
		VALUES ($1, $2, now() + interval '7 days', now())`, token, userID)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// ResolveSession refreshes an unexpired session and returns its verified identity.
func (s *Store) ResolveSession(ctx context.Context, token string) (string, bool, error) {
	var steamID64 string
	err := s.pool.QueryRow(ctx, `
		UPDATE sessions s SET expires_at = now() + interval '7 days'
		FROM users u
		WHERE s.token = $1 AND s.expires_at > now() AND s.user_id = u.id
		RETURNING u.steam_id_64`, token).Scan(&steamID64)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve session: %w", err)
	}
	return steamID64, true, nil
}

// DeleteSession invalidates a session; an unknown token is already invalidated.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token = $1`, token); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// CreateParty atomically makes the creator the leader and sole member of a new party.
func (s *Store) CreateParty(ctx context.Context, leaderUserID int64, name string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin create party: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var partyID string
	if err := tx.QueryRow(ctx, `INSERT INTO parties (leader_id, name) VALUES ($1, $2) RETURNING id`, leaderUserID, name).Scan(&partyID); err != nil {
		return "", fmt.Errorf("create party: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, partyID, leaderUserID); err != nil {
		return "", fmt.Errorf("create leader membership: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit create party: %w", err)
	}
	return partyID, nil
}

// UserParty is a party the user belongs to.
type UserParty struct {
	PartyID string
	Name    string
}

// UserParties lists the user's memberships in stable party creation order.
func (s *Store) UserParties(ctx context.Context, userID int64) ([]UserParty, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT parties.id, parties.name
		FROM memberships JOIN parties ON memberships.party_id = parties.id
		WHERE memberships.user_id = $1
		ORDER BY parties.created_at, parties.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("user parties: %w", err)
	}
	defer rows.Close()
	parties := make([]UserParty, 0)
	for rows.Next() {
		var party UserParty
		if err := rows.Scan(&party.PartyID, &party.Name); err != nil {
			return nil, fmt.Errorf("user parties: scan: %w", err)
		}
		parties = append(parties, party)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("user parties: rows: %w", err)
	}
	return parties, nil
}

// UserPendingInvite identifies a party that has invited the user.
type UserPendingInvite struct {
	PartyID string
	Name    string
}

// UserPendingInvites lists only invitations still awaiting this user's reply.
func (s *Store) UserPendingInvites(ctx context.Context, userID int64) ([]UserPendingInvite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT parties.id, parties.name
		FROM invites JOIN parties ON invites.party_id = parties.id
		WHERE invites.user_id = $1 AND invites.status = 'pending'
		ORDER BY invites.created_at, invites.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("user pending invites: %w", err)
	}
	defer rows.Close()
	invites := make([]UserPendingInvite, 0)
	for rows.Next() {
		var invite UserPendingInvite
		if err := rows.Scan(&invite.PartyID, &invite.Name); err != nil {
			return nil, fmt.Errorf("user pending invites: scan: %w", err)
		}
		invites = append(invites, invite)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("user pending invites: rows: %w", err)
	}
	return invites, nil
}

// PartyMember is a member's stored identity, Steam account id, and medium
// (64px) avatar. SteamID64 is selected by the server-side membership query
// below (users joined through memberships) and never originates from
// request form or query input.
type PartyMember struct {
	UserID    int64
	Name      string
	AvatarURL string
	IsLeader  bool
	SteamID64 string
}

// PartyMembers lists members in seniority order, with user ID breaking ties.
func (s *Store) PartyMembers(ctx context.Context, partyID string) ([]PartyMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT users.id, users.persona_name, users.avatar_medium, users.id = parties.leader_id, users.steam_id_64
		FROM memberships JOIN users ON memberships.user_id = users.id
		JOIN parties ON memberships.party_id = parties.id
		WHERE memberships.party_id = $1
		ORDER BY memberships.joined_at, users.id`, partyID)
	if err != nil {
		return nil, fmt.Errorf("party members: %w", err)
	}
	defer rows.Close()
	members := make([]PartyMember, 0)
	for rows.Next() {
		var member PartyMember
		if err := rows.Scan(&member.UserID, &member.Name, &member.AvatarURL, &member.IsLeader, &member.SteamID64); err != nil {
			return nil, fmt.Errorf("party members: scan: %w", err)
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("party members: rows: %w", err)
	}
	return members, nil
}

// IsMember reports whether the user belongs to the specified party.
func (s *Store) IsMember(ctx context.Context, partyID string, userID int64) (bool, error) {
	var member bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, partyID, userID).Scan(&member); err != nil {
		return false, fmt.Errorf("check membership: %w", err)
	}
	return member, nil
}

// LeaveParty removes the acting user's membership. If the acting user was the
// party's leader and other members remain, leadership passes automatically to
// the earliest-joined remaining member (joined_at, then user_id, as a stable
// tiebreaker). If the leave empties the party of all members, the party row
// itself is deleted, cascading to its memberships and invites.
func (s *Store) LeaveParty(ctx context.Context, partyID string, actingUserID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin leave party: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentLeader int64
	if err := tx.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&currentLeader); err != nil {
		return fmt.Errorf("leave party: lookup party: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE party_id = $1 AND user_id = $2`, partyID, actingUserID); err != nil {
		return fmt.Errorf("leave party: %w", err)
	}

	if currentLeader == actingUserID {
		var successor int64
		err := tx.QueryRow(ctx, `
			SELECT user_id FROM memberships
			WHERE party_id = $1
			ORDER BY joined_at, user_id
			LIMIT 1`, partyID).Scan(&successor)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// No members remain; leave leader_id as-is. The empty-party
			// delete below (independent of who was leader) removes the row.
		case err != nil:
			return fmt.Errorf("leave party: find successor: %w", err)
		default:
			if _, err := tx.Exec(ctx, `UPDATE parties SET leader_id = $1 WHERE id = $2`, successor, partyID); err != nil {
				return fmt.Errorf("leave party: update leader: %w", err)
			}
		}
	}

	// This check is independent of who was leader: a non-leader can also be
	// the last member to leave (e.g. after the leader already departed).
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id = $1`, partyID).Scan(&remaining); err != nil {
		return fmt.Errorf("leave party: count remaining members: %w", err)
	}
	if remaining == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM parties WHERE id = $1`, partyID); err != nil {
			return fmt.Errorf("leave party: delete emptied party: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit leave party: %w", err)
	}
	return nil
}

// StepDown transfers party leadership from the acting leader to a chosen
// member. The acting user must be the party's current leader, the party must
// have at least one other member, and the target must already be a member.
// The former leader remains an ordinary member; no membership rows change.
func (s *Store) StepDown(ctx context.Context, partyID string, actingLeaderUserID int64, targetUserID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin step down: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentLeader int64
	if err := tx.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&currentLeader); err != nil {
		return fmt.Errorf("step down: lookup party: %w", err)
	}
	if currentLeader != actingLeaderUserID {
		return errors.New("step down: acting user is not the party's leader")
	}

	var memberCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id = $1`, partyID).Scan(&memberCount); err != nil {
		return fmt.Errorf("step down: count members: %w", err)
	}
	if memberCount < 2 {
		return errors.New("step down: leader has no other member to hand leadership to")
	}

	var targetIsMember bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, partyID, targetUserID).Scan(&targetIsMember); err != nil {
		return fmt.Errorf("step down: check target membership: %w", err)
	}
	if !targetIsMember {
		return errors.New("step down: target is not a member of the party")
	}

	if _, err := tx.Exec(ctx, `UPDATE parties SET leader_id = $1 WHERE id = $2`, targetUserID, partyID); err != nil {
		return fmt.Errorf("step down: update leader: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit step down: %w", err)
	}
	return nil
}

// InviteCandidate is a known user eligible to appear in a party's invite picker.
type InviteCandidate struct {
	UserID    int64
	Name      string
	AvatarURL string
}

// SendInvite records an invitation only for the current leader. A user can be
// invited again after rejecting. Locking the party serializes this with replies
// and leadership changes.
func (s *Store) SendInvite(ctx context.Context, partyID string, actingLeaderUserID int64, targetUserID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin send invite: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentLeader int64
	if err := tx.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&currentLeader); err != nil {
		return fmt.Errorf("send invite: lookup party: %w", err)
	}
	if currentLeader != actingLeaderUserID {
		return errors.New("send invite: acting user is not the party's leader")
	}

	if _, err := tx.Exec(ctx, `INSERT INTO invites (party_id, user_id, inviter_id, status) VALUES ($1, $2, $3, 'pending')`, partyID, targetUserID, actingLeaderUserID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "invites_pending_party_user_idx" {
			// The failed transaction is rolled back by the defer; the existing
			// pending invitation is unchanged and the repeated send succeeds.
			return nil
		}
		return fmt.Errorf("send invite: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit send invite: %w", err)
	}
	return nil
}

// AcceptInvite atomically joins the acting user and consumes their pending invite.
func (s *Store) AcceptInvite(ctx context.Context, partyID string, actingUserID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin accept invite: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentLeader int64
	if err := tx.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&currentLeader); err != nil {
		return fmt.Errorf("accept invite: lookup party: %w", err)
	}
	var inviteID string
	err = tx.QueryRow(ctx, `SELECT id FROM invites WHERE party_id = $1 AND user_id = $2 AND status = 'pending'`, partyID, actingUserID).Scan(&inviteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("accept invite: no pending invite addressed to acting user")
	}
	if err != nil {
		return fmt.Errorf("accept invite: lookup invite: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, partyID, actingUserID); err != nil {
		return fmt.Errorf("accept invite: create membership: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET status = 'accepted' WHERE id = $1`, inviteID); err != nil {
		return fmt.Errorf("accept invite: update invite: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit accept invite: %w", err)
	}
	return nil
}

// RejectInvite consumes only the acting user's pending invite without creating
// a membership.
func (s *Store) RejectInvite(ctx context.Context, partyID string, actingUserID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin reject invite: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentLeader int64
	if err := tx.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&currentLeader); err != nil {
		return fmt.Errorf("reject invite: lookup party: %w", err)
	}
	var inviteID string
	err = tx.QueryRow(ctx, `SELECT id FROM invites WHERE party_id = $1 AND user_id = $2 AND status = 'pending'`, partyID, actingUserID).Scan(&inviteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("reject invite: no pending invite addressed to acting user")
	}
	if err != nil {
		return fmt.Errorf("reject invite: lookup invite: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET status = 'rejected' WHERE id = $1`, inviteID); err != nil {
		return fmt.Errorf("reject invite: update invite: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit reject invite: %w", err)
	}
	return nil
}

// InviteCandidates intersects live friend identities with registered users,
// excluding self, current members, and pending invitees in the same SQL query.
func (s *Store) InviteCandidates(ctx context.Context, partyID string, actingLeaderUserID int64, friendSteamID64s []string) ([]InviteCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.persona_name, u.avatar_medium FROM users u
		WHERE u.steam_id_64 = ANY($3::text[])
			AND u.id <> $2
			AND EXISTS (SELECT 1 FROM parties p WHERE p.id = $1 AND p.leader_id = $2)
			AND NOT EXISTS (SELECT 1 FROM memberships m WHERE m.party_id = $1 AND m.user_id = u.id)
			AND NOT EXISTS (SELECT 1 FROM invites i WHERE i.party_id = $1 AND i.user_id = u.id AND i.status = 'pending')
		ORDER BY u.id`, partyID, actingLeaderUserID, friendSteamID64s)
	if err != nil {
		return nil, fmt.Errorf("invite candidates: %w", err)
	}
	defer rows.Close()
	candidates := make([]InviteCandidate, 0)
	for rows.Next() {
		var candidate InviteCandidate
		if err := rows.Scan(&candidate.UserID, &candidate.Name, &candidate.AvatarURL); err != nil {
			return nil, fmt.Errorf("invite candidates: scan: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("invite candidates: rows: %w", err)
	}
	return candidates, nil
}

func newSessionToken() (string, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(entropy[:]), nil
}
