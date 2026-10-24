package admin

import (
	"context"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func (s *AdminService) UpdateUser(ctx context.Context, actor domain.User, userID int64, in UpdateUserInput) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	name := strings.TrimSpace(in.Username)
	if userID <= 0 || name == "" {
		return ErrInvalidInput
	}
	if userID == actor.ID && !in.IsAdmin {
		return ErrConflict
	}
	return s.store.UpdateUser(ctx, userID, name, in.IsAdmin)
}

func (s *AdminService) ResetPassword(ctx context.Context, actor domain.User, userID int64, password string) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	if userID <= 0 {
		return ErrInvalidInput
	}
	hash, err := authentication.HashPassword(password)
	if err != nil {
		return ErrInvalidInput
	}
	return s.store.ResetPassword(ctx, userID, hash)
}

func (s *AdminService) DeleteUser(ctx context.Context, actor domain.User, userID int64) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	if userID <= 0 {
		return ErrInvalidInput
	}
	if userID == actor.ID {
		return ErrConflict
	}
	return s.store.DeleteUser(ctx, userID)
}

func (s *AdminService) RenameGroup(ctx context.Context, actor domain.User, groupID int64, name string) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	name = strings.TrimSpace(name)
	if groupID <= 0 || name == "" {
		return ErrInvalidInput
	}
	return s.store.RenameGroup(ctx, groupID, name)
}

func (s *AdminService) DeleteGroup(ctx context.Context, actor domain.User, groupID int64) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	if groupID <= 0 {
		return ErrInvalidInput
	}
	return s.store.DeleteGroup(ctx, groupID)
}

func (s *AdminService) UpdateConnection(ctx context.Context, actor domain.User, in CreateConnectionInput) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	c := in.Connection
	c.Name = strings.TrimSpace(c.Name)
	c.Host = strings.TrimSpace(c.Host)
	c.DBname = strings.TrimSpace(c.DBname)
	c.DBuser = strings.TrimSpace(c.DBuser)
	if c.ID <= 0 || c.Name == "" || c.Host == "" || c.DBname == "" || c.DBuser == "" || c.Port < 1 || c.Port > 65535 {
		return ErrInvalidInput
	}
	switch c.SSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return ErrInvalidInput
	}
	return s.store.UpdateConnection(ctx, c, in.Password)
}

func (s *AdminService) DeleteConnection(ctx context.Context, actor domain.User, dbID int64) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	if dbID <= 0 {
		return ErrInvalidInput
	}
	return s.store.DeleteConnection(ctx, dbID)
}
