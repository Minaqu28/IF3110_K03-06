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
func (this *Store) ValidateSession(ctx context.Context, hash, replacementHash []byte, now time.Time) (authentication.Session, bool, error)
func (this *Store) RevokeSession(ctx context.Context, hash []byte, now time.Time) error
