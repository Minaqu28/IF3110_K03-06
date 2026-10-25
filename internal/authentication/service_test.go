package authentication

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

type fakeStore struct {
	user          domain.User
	findErr       error
	created       []NewSession
	validateCalls int
	validateRet   Session
	validateRot   bool
	validateErr   error
	gotReplace    []byte
	revoked       [][]byte
	revokedIDs    []string
}

func (f *fakeStore) FindUser(context.Context, string) (domain.User, error) { return f.user, f.findErr }
func (f *fakeStore) CreateSession(_ context.Context, s NewSession) error {
	f.created = append(f.created, s)
	return nil
}
func (f *fakeStore) ValidateSession(_ context.Context, _, replacement []byte, _ time.Time) (Session, bool, error) {
	f.validateCalls++
	f.gotReplace = replacement
	return f.validateRet, f.validateRot, f.validateErr
}
func (f *fakeStore) RevokeSession(_ context.Context, hash []byte, _ time.Time) error {
	f.revoked = append(f.revoked, hash)
	return nil
}
func (f *fakeStore) RevokeSessionByID(_ context.Context, sessionID string, _ time.Time) error {
	f.revokedIDs = append(f.revokedIDs, sessionID)
	return nil
}

func activeUser(t *testing.T) domain.User {
	t.Helper()
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	return domain.User{ID: 4, Username: "alice", PasswordHash: hash, IsActive: true}
}

func TestLoginCreatesOpaqueSessionWithAbsoluteExpiry(t *testing.T) {
	now := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	store := &fakeStore{user: activeUser(t)}
	svc := NewService(store, func() time.Time { return now })

	identifier, err := svc.Login(context.Background(), " alice ", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if len(identifier) != 43 {
		t.Errorf("identifier length = %d, want 43 (32 random bytes, base64url)", len(identifier))
	}
	if len(store.created) != 1 {
		t.Fatalf("sessions created = %d, want 1", len(store.created))
	}
	s := store.created[0]
	if !bytes.Equal(s.IdentifierHash, hashIdentifier(identifier)) {
		t.Error("store must receive the hash of the identifier")
	}
	if bytes.Contains(s.IdentifierHash, []byte(identifier)) || s.ID == identifier {
		t.Error("raw identifier must never reach the store")
	}
	// Pinned to the documented policy (README): 12 hours, not the constant under test.
	if !s.AbsoluteExpiresAt.Equal(now.Add(12 * time.Hour)) {
		t.Errorf("absolute expiry = %v, want login time + 12h", s.AbsoluteExpiresAt)
	}
	if s.CSRFToken == "" {
		t.Error("session needs a CSRF token")
	}
}

func TestRenewAfterPrivilegeChangePreservesAbsoluteExpiry(t *testing.T) {
	now := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	absolute := now.Add(2 * time.Hour)
	store := &fakeStore{}
	svc := NewService(store, func() time.Time { return now })

	identifier, err := svc.RenewAfterPrivilegeChange(context.Background(), Session{
		ID:                "old-session",
		User:              domain.User{ID: 4, IsActive: true},
		AbsoluteExpiresAt: absolute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(identifier) != 43 || len(store.created) != 1 {
		t.Fatalf("identifier length=%d created=%d", len(identifier), len(store.created))
	}
	if len(store.revokedIDs) != 1 || store.revokedIDs[0] != "old-session" {
		t.Fatalf("revoked session IDs = %v", store.revokedIDs)
	}
	created := store.created[0]
	if created.UserID != 4 || !created.CreatedAt.Equal(now) || !created.AbsoluteExpiresAt.Equal(absolute) {
		t.Fatalf("renewed session = %+v", created)
	}
	if !bytes.Equal(created.IdentifierHash, hashIdentifier(identifier)) {
		t.Fatal("renewed identifier was not stored by hash")
	}

	if _, err := svc.RenewAfterPrivilegeChange(context.Background(), Session{
		ID:                "expired-session",
		User:              domain.User{ID: 4, IsActive: true},
		AbsoluteExpiresAt: now,
	}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired renewal err=%v, want ErrUnauthenticated", err)
	}
}

func TestLoginIdentifiersAreUnique(t *testing.T) {
	store := &fakeStore{user: activeUser(t)}
	svc := NewService(store, nil)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		id, err := svc.Login(context.Background(), "alice", "correct horse battery")
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatal("duplicate session identifier issued")
		}
		seen[id] = true
	}
}

func TestLoginRejectsBadCredentialsWithoutCreatingSession(t *testing.T) {
	inactive := activeUser(t)
	inactive.IsActive = false
	cases := map[string]struct {
		store    *fakeStore
		password string
	}{
		"wrong password":   {&fakeStore{user: activeUser(t)}, "wrong horse battery"},
		"unknown user":     {&fakeStore{findErr: ErrInvalidCredentials}, "correct horse battery"},
		"inactive account": {&fakeStore{user: inactive}, "correct horse battery"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewService(c.store, nil).Login(context.Background(), "alice", c.password)
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("err = %v, want ErrInvalidCredentials", err)
			}
			if len(c.store.created) != 0 {
				t.Error("a session was created for a failed login")
			}
		})
	}
}

func TestLoginPropagatesInfrastructureErrors(t *testing.T) {
	boom := errors.New("db down")
	_, err := NewService(&fakeStore{findErr: boom}, nil).Login(context.Background(), "alice", "correct horse battery")
	if !errors.Is(err, boom) || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want the store error, not invalid credentials", err)
	}
}

func TestValidateRejectsMalformedIdentifiersWithoutStoreLookup(t *testing.T) {
	for name, id := range map[string]string{
		"empty":      "",
		"too short":  "abc",
		"too long":   strings.Repeat("A", 44),
		"not base64": strings.Repeat("!", 43),
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			if _, err := NewService(store, nil).Validate(context.Background(), id); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
			if store.validateCalls != 0 {
				t.Error("malformed identifier reached the store")
			}
		})
	}
}

func TestValidateReturnsReplacementOnlyWhenRotated(t *testing.T) {
	valid := strings.Repeat("A", 43)

	store := &fakeStore{validateRet: Session{ID: "s"}, validateRot: false}
	got, err := NewService(store, nil).Validate(context.Background(), valid)
	if err != nil || got.RotatedIdentifier != "" {
		t.Fatalf("no rotation: %+v, %v", got, err)
	}

	store = &fakeStore{validateRet: Session{ID: "s"}, validateRot: true}
	got, err = NewService(store, nil).Validate(context.Background(), valid)
	if err != nil {
		t.Fatal(err)
	}
	if got.RotatedIdentifier == "" {
		t.Fatal("rotation must hand the new identifier to the caller")
	}
	if !bytes.Equal(store.gotReplace, hashIdentifier(got.RotatedIdentifier)) {
		t.Error("store was given a different replacement than the one returned")
	}
	if got.RotatedIdentifier == valid {
		t.Error("rotated identifier equals the old one")
	}
}

func TestValidatePropagatesStoreErrors(t *testing.T) {
	store := &fakeStore{validateErr: ErrUnauthenticated}
	if _, err := NewService(store, nil).Validate(context.Background(), strings.Repeat("A", 43)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
}

func TestLogoutRevokesByHash(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, nil)
	if err := svc.Logout(context.Background(), ""); err != nil || len(store.revoked) != 0 {
		t.Fatalf("empty identifier should be a no-op, revoked=%d err=%v", len(store.revoked), err)
	}
	if err := svc.Logout(context.Background(), "some-identifier"); err != nil {
		t.Fatal(err)
	}
	if len(store.revoked) != 1 || !bytes.Equal(store.revoked[0], hashIdentifier("some-identifier")) {
		t.Fatalf("revoked = %v", store.revoked)
	}
}
