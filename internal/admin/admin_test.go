package admin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type fakeStore struct {
	AdminStore
	calls    []string
	levels   []authorization.AccessLevel
	hash     string
	users    []domain.User
	accessFn func() (domain.Connection, authorization.AccessLevel, error)
}

func (f *fakeStore) record(name string) { f.calls = append(f.calls, name) }

func (f *fakeStore) CreateUser(_ context.Context, username, hash string, isAdmin bool) (domain.User, error) {
	f.record("CreateUser")
	f.hash = hash
	return domain.User{ID: 9, Username: username, PasswordHash: hash, IsPlatformAdmin: isAdmin, IsActive: true}, nil
}
func (f *fakeStore) ListUsers(context.Context) ([]domain.User, error) {
	f.record("ListUsers")
	return f.users, nil
}
func (f *fakeStore) UpdateUser(context.Context, int64, string, bool) error {
	f.record("UpdateUser")
	return nil
}
func (f *fakeStore) ResetPassword(_ context.Context, _ int64, hash string) error {
	f.record("ResetPassword")
	f.hash = hash
	return nil
}
func (f *fakeStore) SetUserActive(context.Context, int64, bool) error {
	f.record("SetUserActive")
	return nil
}
func (f *fakeStore) DeleteUser(context.Context, int64) error { f.record("DeleteUser"); return nil }
func (f *fakeStore) CreateGroup(_ context.Context, name string) (domain.Group, error) {
	f.record("CreateGroup")
	return domain.Group{ID: 1, Name: name}, nil
}
func (f *fakeStore) RenameGroup(context.Context, int64, string) error {
	f.record("RenameGroup")
	return nil
}
func (f *fakeStore) DeleteGroup(context.Context, int64) error      { f.record("DeleteGroup"); return nil }
func (f *fakeStore) AddMember(context.Context, int64, int64) error { f.record("AddMember"); return nil }
func (f *fakeStore) RemoveMember(context.Context, int64, int64) error {
	f.record("RemoveMember")
	return nil
}
func (f *fakeStore) CreateConnection(_ context.Context, c domain.Connection, _ string) (domain.Connection, error) {
	f.record("CreateConnection")
	return c, nil
}
func (f *fakeStore) GrantAccess(context.Context, int64, int64, authorization.AccessLevel) error {
	f.record("GrantAccess")
	return nil
}
func (f *fakeStore) RevokeAccess(context.Context, int64, int64) error {
	f.record("RevokeAccess")
	return nil
}
func (f *fakeStore) ListGroups(context.Context) ([]domain.Group, error) {
	f.record("ListGroups")
	return nil, nil
}
func (f *fakeStore) ListMemberships(context.Context) ([]Membership, error) {
	f.record("ListMemberships")
	return nil, nil
}
func (f *fakeStore) ListConnections(context.Context) ([]domain.Connection, error) {
	f.record("ListConnections")
	return nil, nil
}
func (f *fakeStore) ListGrants(context.Context) ([]Grant, error) {
	f.record("ListGrants")
	return nil, nil
}
func (f *fakeStore) AccessLevelsForUser(context.Context, int64, int64) ([]authorization.AccessLevel, error) {
	f.record("AccessLevelsForUser")
	return f.levels, nil
}
func (f *fakeStore) RevokeUserSessions(context.Context, int64) error {
	f.record("RevokeUserSessions")
	return nil
}
func (f *fakeStore) AccessibleConnection(context.Context, int64, int64) (domain.Connection, authorization.AccessLevel, error) {
	f.record("AccessibleConnection")
	return f.accessFn()
}

var (
	adminUser  = domain.User{ID: 1, Username: "root", IsPlatformAdmin: true, IsActive: true}
	plainUser  = domain.User{ID: 2, Username: "bob", IsActive: true}
	frozenAdmn = domain.User{ID: 3, Username: "old", IsPlatformAdmin: true, IsActive: false}
)

func validConnection() CreateConnectionInput {
	return CreateConnectionInput{
		Connection: domain.Connection{Name: "app", Host: "h", Port: 5432, DBname: "d", DBuser: "u", SSLMode: "disable"},
		Password:   "secret",
	}
}

func TestPlatformOperationsRejectNonAdmins(t *testing.T) {
	ctx := context.Background()
	ops := map[string]func(*AdminService, domain.User) error{
		"CreateUser": func(s *AdminService, a domain.User) error {
			_, err := s.CreateUser(ctx, a, CreateUserInput{Username: "x", Password: "long-enough-pw"})
			return err
		},
		"ListUsers": func(s *AdminService, a domain.User) error { _, err := s.ListUsers(ctx, a); return err },
		"UpdateUser": func(s *AdminService, a domain.User) error {
			return s.UpdateUser(ctx, a, 5, UpdateUserInput{Username: "x"})
		},
		"ResetPassword": func(s *AdminService, a domain.User) error { return s.ResetPassword(ctx, a, 5, "long-enough-pw") },
		"SetUserActive": func(s *AdminService, a domain.User) error { return s.SetUserActive(ctx, a, 5, false) },
		"DeleteUser":    func(s *AdminService, a domain.User) error { return s.DeleteUser(ctx, a, 5) },
		"CreateGroup":   func(s *AdminService, a domain.User) error { _, err := s.CreateGroup(ctx, a, "g"); return err },
		"RenameGroup":   func(s *AdminService, a domain.User) error { return s.RenameGroup(ctx, a, 1, "g") },
		"DeleteGroup":   func(s *AdminService, a domain.User) error { return s.DeleteGroup(ctx, a, 1) },
		"AddMember":     func(s *AdminService, a domain.User) error { return s.AddMember(ctx, a, 1, 5) },
		"RemoveMember":  func(s *AdminService, a domain.User) error { return s.RemoveMember(ctx, a, 1, 5) },
		"CreateConnection": func(s *AdminService, a domain.User) error {
			_, err := s.CreateConnection(ctx, a, validConnection())
			return err
		},
		"UpdateConnection": func(s *AdminService, a domain.User) error {
			in := validConnection()
			in.Connection.ID = 1
			return s.UpdateConnection(ctx, a, in)
		},
		"DeleteConnection": func(s *AdminService, a domain.User) error { return s.DeleteConnection(ctx, a, 1) },
		"GrantAccess": func(s *AdminService, a domain.User) error {
			return s.GrantAccess(ctx, a, 1, 1, authorization.AccessViewOnly)
		},
		"RevokeAccess":        func(s *AdminService, a domain.User) error { return s.RevokeAccess(ctx, a, 1, 1) },
		"ListGroups":          func(s *AdminService, a domain.User) error { _, err := s.ListGroups(ctx, a); return err },
		"ListMemberships":     func(s *AdminService, a domain.User) error { _, err := s.ListMemberships(ctx, a); return err },
		"ListConnections":     func(s *AdminService, a domain.User) error { _, err := s.ListConnections(ctx, a); return err },
		"ListGrants":          func(s *AdminService, a domain.User) error { _, err := s.ListGrants(ctx, a); return err },
		"AccessLevelsForUser": func(s *AdminService, a domain.User) error { _, err := s.AccessLevelsForUser(ctx, a, 5, 1); return err },
		"RevokeUserSessions":  func(s *AdminService, a domain.User) error { return s.RevokeUserSessions(ctx, a, 5) },
	}
	for name, op := range ops {
		for who, actor := range map[string]domain.User{"regular user": plainUser, "inactive admin": frozenAdmn} {
			t.Run(name+"/"+who, func(t *testing.T) {
				store := &fakeStore{}
				if err := op(NewService(store, nil), actor); !errors.Is(err, ErrForbidden) {
					t.Fatalf("err = %v, want ErrForbidden", err)
				}
				if len(store.calls) != 0 {
					t.Fatalf("store reached despite rejection: %v", store.calls)
				}
			})
		}
	}
}

func TestCreateUserHashesPasswordAndHidesHash(t *testing.T) {
	store := &fakeStore{}
	user, err := NewService(store, nil).CreateUser(context.Background(), adminUser,
		CreateUserInput{Username: "  usagi  ", Password: "unana yaha yaha"})
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "usagi" {
		t.Errorf("username not trimmed: %q", user.Username)
	}
	if user.PasswordHash != "" {
		t.Error("returned user must not expose the password hash")
	}
	if store.hash == "" || strings.Contains(store.hash, "unana yaha yaha") {
		t.Errorf("store received plaintext or empty hash: %q", store.hash)
	}
	if !authentication.VerifyPassword(store.hash, "unana yaha yaha") {
		t.Error("stored hash does not verify against the password")
	}
}

func TestCreateUserValidation(t *testing.T) {
	cases := map[string]CreateUserInput{
		"blank username":    {Username: "   ", Password: "unana yaha yaha"},
		"short password":    {Username: "usagi", Password: "short"},
		"password too long": {Username: "usagi", Password: strings.Repeat("x", 73)},
		"empty password":    {Username: "usagi", Password: ""},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			_, err := NewService(store, nil).CreateUser(context.Background(), adminUser, in)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
			if len(store.calls) != 0 {
				t.Fatalf("invalid input reached the store: %v", store.calls)
			}
		})
	}
}

func TestListUsersStripsPasswordHashes(t *testing.T) {
	store := &fakeStore{users: []domain.User{{ID: 1, Username: "a", PasswordHash: "h1"}, {ID: 2, Username: "b", PasswordHash: "h2"}}}
	users, err := NewService(store, nil).ListUsers(context.Background(), adminUser)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.PasswordHash != "" {
			t.Errorf("user %s leaked hash", u.Username)
		}
	}
}

func TestResetPasswordHashesNewPassword(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, nil)
	if err := svc.ResetPassword(context.Background(), adminUser, 5, "short"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short password err = %v", err)
	}
	if err := svc.ResetPassword(context.Background(), adminUser, 0, "unana yaha yaha"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad id err = %v", err)
	}
	if err := svc.ResetPassword(context.Background(), adminUser, 5, "unana yaha yaha"); err != nil {
		t.Fatal(err)
	}
	if !authentication.VerifyPassword(store.hash, "unana yaha yaha") {
		t.Error("stored value is not a valid hash of the new password")
	}
}

func TestAdminCannotRemoveOwnAccess(t *testing.T) {
	ctx := context.Background()
	svc := NewService(&fakeStore{}, nil)
	if err := svc.SetUserActive(ctx, adminUser, adminUser.ID, false); !errors.Is(err, ErrConflict) {
		t.Errorf("deactivate self err = %v, want ErrConflict", err)
	}
	if err := svc.UpdateUser(ctx, adminUser, adminUser.ID, UpdateUserInput{Username: "root", IsAdmin: false}); !errors.Is(err, ErrConflict) {
		t.Errorf("demote self err = %v, want ErrConflict", err)
	}
	if err := svc.DeleteUser(ctx, adminUser, adminUser.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("delete self err = %v, want ErrConflict", err)
	}

	store := &fakeStore{}
	svc = NewService(store, nil)
	if err := svc.SetUserActive(ctx, adminUser, 5, false); err != nil {
		t.Errorf("deactivating another user: %v", err)
	}
	if err := svc.UpdateUser(ctx, adminUser, adminUser.ID, UpdateUserInput{Username: "root", IsAdmin: true}); err != nil {
		t.Errorf("renaming self while staying admin: %v", err)
	}
}

func TestCreateConnectionValidation(t *testing.T) {
	mutate := func(f func(*CreateConnectionInput)) CreateConnectionInput { in := validConnection(); f(&in); return in }
	cases := map[string]CreateConnectionInput{
		"blank name":     mutate(func(i *CreateConnectionInput) { i.Connection.Name = " " }),
		"blank host":     mutate(func(i *CreateConnectionInput) { i.Connection.Host = "" }),
		"blank db":       mutate(func(i *CreateConnectionInput) { i.Connection.DBname = "" }),
		"blank user":     mutate(func(i *CreateConnectionInput) { i.Connection.DBuser = "" }),
		"empty password": mutate(func(i *CreateConnectionInput) { i.Password = "" }),
		"port zero":      mutate(func(i *CreateConnectionInput) { i.Connection.Port = 0 }),
		"port too high":  mutate(func(i *CreateConnectionInput) { i.Connection.Port = 65536 }),
		"bad ssl mode":   mutate(func(i *CreateConnectionInput) { i.Connection.SSLMode = "maybe" }),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			if _, err := NewService(store, nil).CreateConnection(context.Background(), adminUser, in); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
			if len(store.calls) != 0 {
				t.Fatalf("invalid connection reached the store: %v", store.calls)
			}
		})
	}
	store := &fakeStore{}
	if _, err := NewService(store, nil).CreateConnection(context.Background(), adminUser, validConnection()); err != nil {
		t.Fatalf("valid connection rejected: %v", err)
	}
}

func TestGrantAccessRejectsUnknownLevels(t *testing.T) {
	for _, level := range []authorization.AccessLevel{authorization.AccessNone, "ADMIN", "view_edit"} {
		store := &fakeStore{}
		if err := NewService(store, nil).GrantAccess(context.Background(), adminUser, 1, 1, level); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("level %q: err = %v, want ErrInvalidInput", level, err)
		}
		if len(store.calls) != 0 {
			t.Errorf("level %q reached the store", level)
		}
	}
	for _, level := range []authorization.AccessLevel{authorization.AccessViewOnly, authorization.AccessViewEdit} {
		if err := NewService(&fakeStore{}, nil).GrantAccess(context.Background(), adminUser, 1, 1, level); err != nil {
			t.Errorf("level %q rejected: %v", level, err)
		}
	}
}

func TestAccessForUsesHighestGroupGrant(t *testing.T) {
	cases := []struct {
		name   string
		levels []authorization.AccessLevel
		want   authorization.AccessLevel
	}{
		{"no grants", nil, authorization.AccessNone},
		{"view only", []authorization.AccessLevel{authorization.AccessViewOnly}, authorization.AccessViewOnly},
		{"edit wins over view", []authorization.AccessLevel{authorization.AccessViewOnly, authorization.AccessViewEdit}, authorization.AccessViewEdit},
		{"order does not matter", []authorization.AccessLevel{authorization.AccessViewEdit, authorization.AccessViewOnly}, authorization.AccessViewEdit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NewService(&fakeStore{levels: c.levels}, nil).AccessFor(context.Background(), plainUser, 1)
			if err != nil || got != c.want {
				t.Fatalf("AccessFor = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestInactiveUserHasNoAccess(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{levels: []authorization.AccessLevel{authorization.AccessViewEdit}}
	svc := NewService(store, nil)
	inactive := domain.User{ID: 7, IsActive: false}

	if level, _ := svc.AccessFor(ctx, inactive, 1); level != authorization.AccessNone {
		t.Errorf("AccessFor = %q, want none", level)
	}
	if _, err := svc.ListAccessibleConnections(ctx, inactive); !errors.Is(err, ErrForbidden) {
		t.Errorf("ListAccessibleConnections err = %v, want ErrForbidden", err)
	}
	if _, _, err := svc.ConnectionWithAccess(ctx, inactive, 1); !errors.Is(err, ErrForbidden) {
		t.Errorf("ConnectionWithAccess err = %v, want ErrForbidden", err)
	}
	if len(store.calls) != 0 {
		t.Errorf("inactive user reached the store: %v", store.calls)
	}
}

func TestConnectionWithAccessHidesMissingDatabases(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{accessFn: func() (domain.Connection, authorization.AccessLevel, error) {
		return domain.Connection{}, authorization.AccessNone, ErrNotFound
	}}
	svc := NewService(store, nil)
	if _, _, err := svc.ConnectionWithAccess(ctx, plainUser, 99); !errors.Is(err, ErrForbidden) {
		t.Errorf("not found mapped to %v, want ErrForbidden", err)
	}
	if _, _, err := svc.ConnectionWithAccess(ctx, plainUser, 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("id 0 err = %v, want ErrForbidden", err)
	}

	boom := errors.New("db down")
	store.accessFn = func() (domain.Connection, authorization.AccessLevel, error) {
		return domain.Connection{}, authorization.AccessNone, boom
	}
	if _, _, err := svc.ConnectionWithAccess(ctx, plainUser, 1); !errors.Is(err, boom) {
		t.Errorf("infrastructure error swallowed: %v", err)
	}

	store.accessFn = func() (domain.Connection, authorization.AccessLevel, error) {
		return domain.Connection{ID: 1, Name: "app"}, authorization.AccessViewOnly, nil
	}
	conn, level, err := svc.ConnectionWithAccess(ctx, plainUser, 1)
	if err != nil || conn.ID != 1 || level != authorization.AccessViewOnly {
		t.Errorf("happy path = %+v, %q, %v", conn, level, err)
	}
}
