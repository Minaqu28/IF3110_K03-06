package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
)

func BootstrapAdmin(ctx context.Context, db *sql.DB, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return errors.New("bootstrap username and password are required")
	}
	hash, err := authentication.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO users (username, password_hash, is_platform_admin)
		VALUES ($1, $2, true) ON CONFLICT (username) DO NOTHING`, username, hash)
	return err
}
