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

var ErrForbidden = errors.New("forbidden operation")
var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("operation is conflicted")
var ErrInvalidInput = errors.New("input invalid")

type AdminStore interface {
	CreateUser(ctx context.Context, username, passwordHash string, isAdmin bool) (domain.User, error)
	ListUsers(ctx context.Context) ([]domain.User, error)
	UpdateUser(ctx context.Context, userID int64, username string, isAdmin bool) error
	ResetPassword(ctx context.Context, userID int64, passwordHash string) error
	SetUserActive(ctx context.Context, userID int64, active bool) error
	DeleteUser(ctx context.Context, userID int64) error

	CreateGroup(ctx context.Context, name string) (domain.Group, error)
	ListGroups(ctx context.Context) ([]domain.Group, error)
	RenameGroup(ctx context.Context, groupID int64, name string) error
	SetGroupLeader(ctx context.Context, groupID int64, leaderID *int64) error
	DeleteGroup(ctx context.Context, groupID int64) error
	AddMember(ctx context.Context, groupID, userID int64) error
	RemoveMember(ctx context.Context, groupID, userID int64) error
	ListMemberships(ctx context.Context) ([]Membership, error)

	CreateConnection(ctx context.Context, c domain.Connection, password string) (domain.Connection, error)
	UpdateConnection(ctx context.Context, c domain.Connection, password string) error
	DeleteConnection(ctx context.Context, dbID int64) error
	ListConnections(ctx context.Context) ([]domain.Connection, error)
	AccessibleConnections(ctx context.Context, userID int64) ([]domain.Connection, error)
	AccessibleConnection(ctx context.Context, userID, dbID int64) (domain.Connection, authorization.AccessLevel, error)
	GetConnection(ctx context.Context, dbID int64) (domain.Connection, error)
	GrantAccess(ctx context.Context, groupID, dbID int64, level authorization.AccessLevel) error
	RevokeAccess(ctx context.Context, groupID, dbID int64) error
	ListGrants(ctx context.Context) ([]Grant, error)

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

type CreateConnectionInput struct {
	Connection domain.Connection
	Password   string
}

type UpdateUserInput struct {
	Username string
	IsAdmin  bool
}

type Membership struct {
	GroupID   int64
	GroupName string
	UserID    int64
	Username  string
}

type Grant struct {
	GroupID      int64
	GroupName    string
	DatabaseID   int64
	DatabaseName string
	Level        authorization.AccessLevel
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
	for i := range users {
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
	return this.store.SetUserActive(ctx, userID, active)
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

func (this *AdminService) CreateConnection(ctx context.Context, actor domain.User, in CreateConnectionInput) (domain.Connection, error) {
	if !authorization.CanManagePlatform(actor) {
		return domain.Connection{}, ErrForbidden
	}
	c := in.Connection
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return domain.Connection{}, ErrInvalidInput
	}
	c.DBname = strings.TrimSpace(c.DBname)
	if c.DBname == "" {
		return domain.Connection{}, ErrInvalidInput
	}
	c.DBuser = strings.TrimSpace(c.DBuser)
	if c.DBuser == "" {
		return domain.Connection{}, ErrInvalidInput
	}
	c.Host = strings.TrimSpace(c.Host)
	if c.Host == "" || in.Password == "" {
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
	return this.store.CreateConnection(ctx, c, in.Password)
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

func (s *AdminService) SetGroupLeader(ctx context.Context, actor domain.User, groupID int64, leaderID *int64) error {
	if !authorization.CanManagePlatform(actor) {
		return ErrForbidden
	}
	if groupID <= 0 {
		return ErrInvalidInput
	}
	if leaderID != nil && *leaderID <= 0 {
		return ErrInvalidInput
	}
	return s.store.SetGroupLeader(ctx, groupID, leaderID)
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

func (s *AdminService) ListGroups(ctx context.Context, actor domain.User) ([]domain.Group, error) {
	if !authorization.CanManagePlatform(actor) {
		return nil, ErrForbidden
	}
	return s.store.ListGroups(ctx)
}

func (s *AdminService) ListMemberships(ctx context.Context, actor domain.User) ([]Membership, error) {
	if !authorization.CanManagePlatform(actor) {
		return nil, ErrForbidden
	}
	return s.store.ListMemberships(ctx)
}

func (s *AdminService) ListConnections(ctx context.Context, actor domain.User) ([]domain.Connection, error) {
	if !authorization.CanManagePlatform(actor) {
		return nil, ErrForbidden
	}
	return s.store.ListConnections(ctx)
}

func (s *AdminService) ListGrants(ctx context.Context, actor domain.User) ([]Grant, error) {
	if !authorization.CanManagePlatform(actor) {
		return nil, ErrForbidden
	}
	return s.store.ListGrants(ctx)
}

func (s *AdminService) ListAccessibleConnections(ctx context.Context, actor domain.User) ([]domain.Connection, error) {
	if !actor.IsActive {
		return nil, ErrForbidden
	}
	return s.store.AccessibleConnections(ctx, actor.ID)
}

func (s *AdminService) ConnectionFor(ctx context.Context, actor domain.User, dbID int64) (domain.Connection, error) {
	connection, _, err := s.ConnectionWithAccess(ctx, actor, dbID)
	return connection, err
}

func (s *AdminService) ConnectionWithAccess(ctx context.Context, actor domain.User, dbID int64) (domain.Connection, authorization.AccessLevel, error) {
	if dbID <= 0 || !actor.IsActive {
		return domain.Connection{}, authorization.AccessNone, ErrForbidden
	}
	connection, level, err := s.store.AccessibleConnection(ctx, actor.ID, dbID)
	if err == ErrNotFound {
		return domain.Connection{}, authorization.AccessNone, ErrForbidden
	}
	if err != nil {
		return domain.Connection{}, authorization.AccessNone, err
	}
	return connection, level, nil
}
