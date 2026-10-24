package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
)

type AdminPostgresStore struct {
	db  *sql.DB
	key []byte
}

var _ admin.AdminStore = (*AdminPostgresStore)(nil)

func NewAdminPostgresStore(db *sql.DB, encodedKey string) (*AdminPostgresStore, error) {
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("DB_CREDENTIAL_KEY must be a base64-encoded 32-byte key")
	}
	return &AdminPostgresStore{db: db, key: key}, nil
}

func (s *AdminPostgresStore) encryptPassword(password string) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(password), nil), nil
}

func (s *AdminPostgresStore) DecryptPassword(encrypted []byte) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(encrypted) < aead.NonceSize() {
		return "", errors.New("invalid encrypted credential")
	}
	password, err := aead.Open(nil, encrypted[:aead.NonceSize()], encrypted[aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("invalid encrypted credential")
	}
	return string(password), nil
}

func adminError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return admin.ErrConflict
		case "23503":
			return admin.ErrNotFound
		case "23502", "23514", "22001":
			return admin.ErrInvalidInput
		}
	}
	return err
}

func rowsAffected(result sql.Result, err error) error {
	if err != nil {
		return adminError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return admin.ErrNotFound
	}
	return nil
}

func (s *AdminPostgresStore) CreateUser(ctx context.Context, username, passwordHash string, isAdmin bool) (domain.User, error) {
	u := domain.User{Username: username, IsPlatformAdmin: isAdmin}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO users (username, password_hash, is_platform_admin)
		VALUES ($1, $2, $3) RETURNING id, is_active`, username, passwordHash, isAdmin).
		Scan(&u.ID, &u.IsActive)
	return u, adminError(err)
}

func (s *AdminPostgresStore) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, is_platform_admin, is_active FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.Username, &u.IsPlatformAdmin, &u.IsActive); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *AdminPostgresStore) UpdateUser(ctx context.Context, userID int64, username string, isAdmin bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE users SET username = $2, is_platform_admin = $3 WHERE id = $1`, userID, username, isAdmin)
	if err := rowsAffected(result, err); err != nil {
		return err
	}
	if err := revokeUserSessions(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) ResetPassword(ctx context.Context, userID int64, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, passwordHash)
	if err := rowsAffected(result, err); err != nil {
		return err
	}
	if err := revokeUserSessions(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) SetUserActive(ctx context.Context, userID int64, active bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE users SET is_active = $2 WHERE id = $1`, userID, active)
	if err := rowsAffected(result, err); err != nil {
		return err
	}
	if !active {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) DeleteUser(ctx context.Context, userID int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	return rowsAffected(result, err)
}

func (s *AdminPostgresStore) CreateGroup(ctx context.Context, name string) (domain.Group, error) {
	g := domain.Group{Name: name}
	err := s.db.QueryRowContext(ctx, `INSERT INTO groups (name) VALUES ($1) RETURNING id`, name).Scan(&g.ID)
	return g, adminError(err)
}

func (s *AdminPostgresStore) ListGroups(ctx context.Context) ([]domain.Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []domain.Group
	for rows.Next() {
		var g domain.Group
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s *AdminPostgresStore) RenameGroup(ctx context.Context, groupID int64, name string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE groups SET name = $2 WHERE id = $1`, groupID, name)
	return rowsAffected(result, err)
}

func (s *AdminPostgresStore) DeleteGroup(ctx context.Context, groupID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := revokeGroupSessions(ctx, tx, groupID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
	if err := rowsAffected(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) AddMember(ctx context.Context, groupID, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, groupID, userID)
	if err != nil {
		return adminError(err)
	}
	if err := revokeUserSessions(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) RemoveMember(ctx context.Context, groupID, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID)
	if err := rowsAffected(result, err); err != nil {
		return err
	}
	if err := revokeUserSessions(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) ListMemberships(ctx context.Context) ([]admin.Membership, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT gm.group_id, g.name, gm.user_id, u.username
		FROM group_members gm JOIN groups g ON g.id = gm.group_id
		JOIN users u ON u.id = gm.user_id ORDER BY g.name, u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var memberships []admin.Membership
	for rows.Next() {
		var m admin.Membership
		if err := rows.Scan(&m.GroupID, &m.GroupName, &m.UserID, &m.Username); err != nil {
			return nil, err
		}
		memberships = append(memberships, m)
	}
	return memberships, rows.Err()
}

func (s *AdminPostgresStore) CreateConnection(ctx context.Context, c domain.Connection, password string) (domain.Connection, error) {
	encrypted, err := s.encryptPassword(password)
	if err != nil {
		return domain.Connection{}, err
	}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO database_connections (name, host, port, db_name, db_user, password_encrypted, ssl_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		c.Name, c.Host, c.Port, c.DBname, c.DBuser, encrypted, c.SSLMode).Scan(&c.ID)
	return c, adminError(err)
}

func (s *AdminPostgresStore) UpdateConnection(ctx context.Context, c domain.Connection, password string) error {
	var encrypted any
	if password != "" {
		ciphertext, err := s.encryptPassword(password)
		if err != nil {
			return err
		}
		encrypted = ciphertext
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE database_connections
		SET name = $2, host = $3, port = $4, db_name = $5, db_user = $6,
		    password_encrypted = COALESCE($7, password_encrypted), ssl_mode = $8, updated_at = now()
		WHERE id = $1`, c.ID, c.Name, c.Host, c.Port, c.DBname, c.DBuser, encrypted, c.SSLMode)
	if err := rowsAffected(result, err); err != nil {
		return err
	}
	if err := revokeConnectionSessions(ctx, tx, c.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) DeleteConnection(ctx context.Context, dbID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := revokeConnectionSessions(ctx, tx, dbID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM database_connections WHERE id = $1`, dbID)
	if err := rowsAffected(result, err); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return admin.ErrConflict
		}
		return err
	}
	return tx.Commit()
}

const connectionColumns = `id, name, host, port, db_name, db_user, ssl_mode`

func scanConnections(rows *sql.Rows) ([]domain.Connection, error) {
	var connections []domain.Connection
	for rows.Next() {
		var c domain.Connection
		if err := rows.Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.DBname, &c.DBuser, &c.SSLMode); err != nil {
			return nil, err
		}
		connections = append(connections, c)
	}
	return connections, rows.Err()
}

func (s *AdminPostgresStore) ListConnections(ctx context.Context) ([]domain.Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connectionColumns+` FROM database_connections ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConnections(rows)
}

func (s *AdminPostgresStore) AccessibleConnections(ctx context.Context, userID int64) ([]domain.Connection, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT c.id, c.name, c.host, c.port, c.db_name, c.db_user, c.ssl_mode
		FROM database_connections c
		JOIN group_database_access a ON a.database_id = c.id
		JOIN group_members gm ON gm.group_id = a.group_id
		WHERE gm.user_id = $1 ORDER BY c.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConnections(rows)
}

func (s *AdminPostgresStore) AccessibleConnection(ctx context.Context, userID, dbID int64) (domain.Connection, authorization.AccessLevel, error) {
	var c domain.Connection
	var level authorization.AccessLevel
	err := s.db.QueryRowContext(ctx, `
		SELECT c.id, c.name, c.host, c.port, c.db_name, c.db_user, c.ssl_mode,
		       CASE WHEN bool_or(a.access_level = 'VIEW_EDIT') THEN 'VIEW_EDIT' ELSE 'VIEW_ONLY' END
		FROM database_connections c
		JOIN group_database_access a ON a.database_id = c.id
		JOIN group_members gm ON gm.group_id = a.group_id
		WHERE gm.user_id = $1 AND c.id = $2
		GROUP BY c.id`, userID, dbID).
		Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.DBname, &c.DBuser, &c.SSLMode, &level)
	return c, level, adminError(err)
}

func (s *AdminPostgresStore) GetConnection(ctx context.Context, dbID int64) (domain.Connection, error) {
	var c domain.Connection
	err := s.db.QueryRowContext(ctx, `SELECT `+connectionColumns+` FROM database_connections WHERE id = $1`, dbID).
		Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.DBname, &c.DBuser, &c.SSLMode)
	return c, adminError(err)
}

func (s *AdminPostgresStore) GrantAccess(ctx context.Context, groupID, dbID int64, level authorization.AccessLevel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO group_database_access (group_id, database_id, access_level)
		VALUES ($1, $2, $3)
		ON CONFLICT (group_id, database_id) DO UPDATE SET access_level = EXCLUDED.access_level, granted_at = now()`,
		groupID, dbID, level)
	if err != nil {
		return adminError(err)
	}
	if err := revokeGroupSessions(ctx, tx, groupID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) RevokeAccess(ctx context.Context, groupID, dbID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM group_database_access WHERE group_id = $1 AND database_id = $2`, groupID, dbID)
	if err := rowsAffected(result, err); err != nil {
		return err
	}
	if err := revokeGroupSessions(ctx, tx, groupID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AdminPostgresStore) ListGrants(ctx context.Context) ([]admin.Grant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.group_id, g.name, a.database_id, c.name, a.access_level
		FROM group_database_access a JOIN groups g ON g.id = a.group_id
		JOIN database_connections c ON c.id = a.database_id
		ORDER BY g.name, c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []admin.Grant
	for rows.Next() {
		var g admin.Grant
		if err := rows.Scan(&g.GroupID, &g.GroupName, &g.DatabaseID, &g.DatabaseName, &g.Level); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

func (s *AdminPostgresStore) AccessLevelsForUser(ctx context.Context, userID, dbID int64) ([]authorization.AccessLevel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.access_level FROM group_database_access a
		JOIN group_members gm ON gm.group_id = a.group_id
		WHERE gm.user_id = $1 AND a.database_id = $2`, userID, dbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var levels []authorization.AccessLevel
	for rows.Next() {
		var level authorization.AccessLevel
		if err := rows.Scan(&level); err != nil {
			return nil, err
		}
		levels = append(levels, level)
	}
	return levels, rows.Err()
}

func revokeUserSessions(ctx context.Context, tx *sql.Tx, userID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

func revokeGroupSessions(ctx context.Context, tx *sql.Tx, groupID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE user_id IN (SELECT user_id FROM group_members WHERE group_id = $1)
		  AND revoked_at IS NULL`, groupID)
	return err
}

func revokeConnectionSessions(ctx context.Context, tx *sql.Tx, dbID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE user_id IN (
			SELECT gm.user_id FROM group_members gm
			JOIN group_database_access a ON a.group_id = gm.group_id
			WHERE a.database_id = $1
		) AND revoked_at IS NULL`, dbID)
	return err
}

func (s *AdminPostgresStore) RevokeUserSessions(ctx context.Context, userID int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	_, err = result.RowsAffected()
	return err
}
