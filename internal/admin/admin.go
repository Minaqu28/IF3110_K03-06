package admin

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

// ini gw tulis seadanya dulu le, tar integrate ke error HTTP beneran
var ErrForbidden = errors.New("forbidden operation")
var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("operation is conflicted")
var ErrInvalidInput = errors.New("input invalid")

type AdminStore interface {
	CreateUser(ctx context.Context, username, passwordHash string, isAdmin bool) (domain.User, error)
	ListUsers(ctx context.Context) ([]domain.User, error)
	SetUserActive(ctx context.Context, userID int64, active bool) error

	CreateGroup(ctx context.Context, name string) (domain.Group, error)
	AddMember(ctx context.Context, groupID, userID int64) error
	RemoveMember(ctx context.Context, groupID, userID int64) error

	CreateConnection(ctx context.Context, c domain.Connection) (domain.Connection, error)
	GrantAccess(ctx context.Context, groupID, dbID int64, level authorization.AccessLevel) error
	RevokeAccess(ctx context.Context, groupID, dbID int64) error

	AccessLevelsForUser(ctx context.Context, userID, dbID int64) ([]authorization.AccessLevel, error)
	RevokeUserSessions(ctx context.Context, userID int64) error
}

type AdminService struct {
	store AdminStore
	now   func() time.Time
}

func NewService(store AdminStore, now func() time.Time) *AdminService {
	if now == nil {
		now = time.Now
	}
	return &AdminService{store: store, now: now}
}

type CreateUserInput struct {
	Username string
	Password string
	IsAdmin  bool
}

func (this *AdminService) CreateUser(ctx context.Context, actor domain.User, in CreateUserInput) (domain.User, error) {
	if !authorization.CanManagePlatform(actor) {
		return domain.User{}, ErrForbidden
	}
	username := strings.TrimSpace(in.Username)
	if username == "" {
		return domain.User{}, ErrInvalidInput
	}
	hash, err := authentication.HashPassword(in.Password)
	if err != nil {
		return domain.User{}, ErrInvalidInput
	}
	u, err := this.store.CreateUser(ctx, username, hash, in.IsAdmin)
	u.PasswordHash = ""
	return u, err
}

func (this *AdminService) ListUsers(ctx context.Context, actor domain.User) ([]domain.User, error) {
	if !authorization.CanManagePlatform(actor) {
		return []domain.User{}, ErrForbidden
	}
	users, err := this.store.ListUsers(ctx)
	for i, _ := range users {
		users[i].PasswordHash = ""
	}
	return users, err
}

func (this *AdminService) SetUserActive(ctx context.Context, actor domain.User, userID int64, active bool) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	if !active && userID == actor.ID {
		return ErrConflict
	}
	if err := this.store.SetUserActive(ctx, userID, active); err != nil {
		return err
	}
	if !active {
		return this.store.RevokeUserSessions(ctx, userID)
	}
	return nil
}

func (this *AdminService) CreateGroup(ctx context.Context, actor domain.User, name string) (domain.Group, error) {
	if !authorization.CanManagePlatform(actor) {
		return domain.Group{}, ErrForbidden
	}
	trimmedname := strings.TrimSpace(name)
	if trimmedname == "" {
		return domain.Group{}, ErrInvalidInput
	}
	return this.store.CreateGroup(ctx, trimmedname)
}
func (this *AdminService) AddMember(ctx context.Context, actor domain.User, groupID, userID int64) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	return this.store.AddMember(ctx, groupID, userID)
}
func (this *AdminService) RemoveMember(ctx context.Context, actor domain.User, groupID, userID int64) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	return this.store.RemoveMember(ctx, groupID, userID)
}

func (this *AdminService) CreateConnection(ctx context.Context, actor domain.User, c domain.Connection) (domain.Connection, error) {
	if !authorization.CanManagePlatform(actor) {
		return domain.Connection{}, ErrForbidden
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return domain.Connection{}, ErrInvalidInput
	}
	c.Db_name = strings.TrimSpace(c.Db_name)
	if c.Db_name == "" {
		return domain.Connection{}, ErrInvalidInput
	}
	c.Db_user = strings.TrimSpace(c.Db_user)
	if c.Db_user == "" {
		return domain.Connection{}, ErrInvalidInput
	}
	if c.Host == "" {
		return domain.Connection{}, ErrInvalidInput
	}
	if c.Port < 1 || c.Port > 65535 {
		return domain.Connection{}, ErrInvalidInput
	}
	switch c.SSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return domain.Connection{}, ErrInvalidInput
	}
	return this.store.CreateConnection(ctx, c)
}
func (this *AdminService) GrantAccess(ctx context.Context, actor domain.User, groupID, dbID int64, level authorization.AccessLevel) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	if level != authorization.AccessViewOnly && level != authorization.AccessViewEdit {
		return ErrInvalidInput
	}
	return this.store.GrantAccess(ctx, groupID, dbID, level)
}
func (this *AdminService) RevokeAccess(ctx context.Context, actor domain.User, groupID, dbID int64) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	return this.store.RevokeAccess(ctx, groupID, dbID)
}

func (this *AdminService) AccessFor(ctx context.Context, actor domain.User, dbID int64) (authorization.AccessLevel, error) {
	if !actor.IsActive {
		return authorization.AccessNone, nil
	}
	levels, err := this.store.AccessLevelsForUser(ctx, actor.ID, dbID)
	if err != nil {
		return authorization.AccessNone, err
	}
	return authorization.EffectiveAccess(levels...), nil
}

func (this *AdminService) AccessLevelsForUser(ctx context.Context, actor domain.User, userID, dbID int64) (authorization.AccessLevel, error) {
	if !authorization.CanManagePlatform(actor) {
		return authorization.AccessNone, ErrForbidden
	}
	levels, err := this.store.AccessLevelsForUser(ctx, userID, dbID)
	if err != nil {
		return authorization.AccessNone, err
	}
	return authorization.EffectiveAccess(levels...), nil
}
func (this *AdminService) RevokeUserSessions(ctx context.Context, actor domain.User, userID int64) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	return this.store.RevokeUserSessions(ctx, userID)
}
