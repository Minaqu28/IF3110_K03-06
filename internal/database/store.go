package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type Store struct {
	db *sql.DB
}

var _ authentication.Store = (*Store)(nil)

func (this *Store) FindUser(ctx context.Context, username string) (domain.User, error) {
	var user domain.User
	err := this.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_platform_admin, is_active
		FROM users WHERE username = $1
	`, username).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.IsPlatformAdmin, &user.IsActive)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, authentication.ErrInvalidCredentials
	}
	return user, err
}
func (this *Store) CreateSession(ctx context.Context, session authentication.NewSession) error {
	_, err := this.db.ExecContext(ctx, `
		INSERT INTO sessions
			(id, user_id, identifier_hash, csrf_token, created_at, rotated_at, last_seen_at, absolute_expires_at)
		VALUES ($1, $2, $3, $4, $5, $5, $5, $6)
	`, session.ID, session.UserID, session.IdentifierHash, session.CSRFToken, session.CreatedAt, session.AbsoluteExpiresAt)
	return err
}
func (this *Store) ValidateSession(ctx context.Context, hash, replacementHash []byte, now time.Time) (authentication.Session, bool, error) {
	tx, err := this.db.BeginTx(ctx, nil)
	if err != nil {
		return authentication.Session{}, false, err
	}
	defer tx.Rollback()
	var (
		sessionID, csrf string
		currentHash     []byte
		state           authentication.SessionState
		user            domain.User
	)
	err = tx.QueryRowContext(ctx, `
		SELECT s.id, s.csrf_token, s.identifier_hash,
		       s.rotated_at, s.last_seen_at, s.absolute_expires_at, s.revoked_at IS NOT NULL,
		       u.id, u.username, u.password_hash, u.is_platform_admin, u.is_active
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.identifier_hash = $1 OR s.previous_hash = $1
		FOR UPDATE OF s`, hash,
	).Scan(&sessionID, &csrf, &currentHash,
		&state.RotatedAt, &state.LastSeenAt, &state.AbsoluteExpiresAt, &state.Revoked,
		&user.ID, &user.Username, &user.PasswordHash, &user.IsPlatformAdmin, &user.IsActive)
	if errors.Is(err, sql.ErrNoRows) {
		return authentication.Session{}, false, authentication.ErrUnauthenticated
	}
	if err != nil {
		return authentication.Session{}, false, err
	}
	state.UserActive = user.IsActive
	state.MatchedPrevious = !bytes.Equal(currentHash, hash)
	var rotated bool
	switch authentication.Decide(state, now) {
	case authentication.Reject:
		return authentication.Session{}, false, authentication.ErrUnauthenticated
	case authentication.AcceptAndRotate:
		rotated = true
		_, err = tx.ExecContext(ctx, `
			UPDATE sessions
			SET previous_hash = identifier_hash, identifier_hash = $2, rotated_at = $3, last_seen_at = $3
			WHERE id = $1`, sessionID, replacementHash, now)
	default:
		_, err = tx.ExecContext(ctx,
			`UPDATE sessions SET last_seen_at = $2 WHERE id = $1`,
			sessionID, now)
	}
	if err != nil {
		return authentication.Session{}, false, err
	}
	err = tx.Commit()
	if err != nil {
		return authentication.Session{}, false, err
	}
	return authentication.Session{
		ID:        sessionID,
		User:      user,
		CSRFToken: csrf,
		Previous:  state.MatchedPrevious,
	}, rotated, nil
}
func (this *Store) RevokeSession(ctx context.Context, hash []byte, now time.Time) error {
	_, err := this.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = $2
	WHERE (identifier_hash = $1 OR previous_hash = $1) AND revoked_at IS NULL`,
		hash, now)
	return err
}
