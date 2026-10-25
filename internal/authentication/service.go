package authentication

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"golang.org/x/crypto/bcrypt"
)

const (
	IdleTimeout      = 30 * time.Minute
	AbsoluteLifetime = 12 * time.Hour
	RotationInterval = 15 * time.Minute
	PreviousGrace    = 30 * time.Second
)

var ErrInvalidCredentials = errors.New("invalid credentials")
var ErrUnauthenticated = errors.New("unauthenticated")

type Session struct {
	ID                string
	User              domain.User
	CSRFToken         string
	AbsoluteExpiresAt time.Time
	Previous          bool
	RotatedIdentifier string
}

type NewSession struct {
	ID                string
	UserID            int64
	IdentifierHash    []byte
	CSRFToken         string
	CreatedAt         time.Time
	AbsoluteExpiresAt time.Time
}

type Store interface {
	FindUser(ctx context.Context, username string) (domain.User, error)
	CreateSession(ctx context.Context, session NewSession) error
	ValidateSession(ctx context.Context, hash, replacementHash []byte, now time.Time) (Session, bool, error)
	RevokeSession(ctx context.Context, hash []byte, now time.Time) error
	RevokeSessionByID(ctx context.Context, sessionID string, now time.Time) error
}

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}
}

func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 72 {
		return "", errors.New("password must be 12 to 72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func token(n int) (string, error) {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func hashIdentifier(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func (s *Service) Login(ctx context.Context, username, password string) (string, error) {
	user, err := s.store.FindUser(ctx, strings.TrimSpace(username))
	if err != nil && !errors.Is(err, ErrInvalidCredentials) {
		return "", err
	}
	if err != nil || !user.IsActive {
		return "", ErrInvalidCredentials
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return "", ErrInvalidCredentials
	}
	now := s.now().UTC()
	return s.issueSession(ctx, user.ID, now, now.Add(AbsoluteLifetime))
}

func (s *Service) issueSession(ctx context.Context, userID int64, now, absoluteExpiresAt time.Time) (string, error) {
	id, err := token(16)
	if err != nil {
		return "", err
	}
	identifier, err := token(32)
	if err != nil {
		return "", err
	}
	csrf, err := token(32)
	if err != nil {
		return "", err
	}
	err = s.store.CreateSession(ctx, NewSession{ID: id, UserID: userID, IdentifierHash: hashIdentifier(identifier), CSRFToken: csrf, CreatedAt: now, AbsoluteExpiresAt: absoluteExpiresAt})
	if err != nil {
		return "", err
	}
	return identifier, nil
}

func (s *Service) RenewAfterPrivilegeChange(ctx context.Context, session Session) (string, error) {
	now := s.now().UTC()
	if session.ID == "" || session.User.ID <= 0 || !session.User.IsActive || session.AbsoluteExpiresAt.IsZero() || !now.Before(session.AbsoluteExpiresAt) {
		return "", ErrUnauthenticated
	}
	if err := s.store.RevokeSessionByID(ctx, session.ID, now); err != nil {
		return "", err
	}
	return s.issueSession(ctx, session.User.ID, now, session.AbsoluteExpiresAt)
}

func (s *Service) Validate(ctx context.Context, identifier string) (Session, error) {
	if len(identifier) != 43 {
		return Session{}, ErrUnauthenticated
	}
	if _, err := base64.RawURLEncoding.DecodeString(identifier); err != nil {
		return Session{}, ErrUnauthenticated
	}
	replacement, err := token(32)
	if err != nil {
		return Session{}, err
	}
	session, rotated, err := s.store.ValidateSession(ctx, hashIdentifier(identifier), hashIdentifier(replacement), s.now().UTC())
	if err != nil {
		return Session{}, err
	}
	if rotated {
		session.RotatedIdentifier = replacement
	}
	return session, nil
}

func (s *Service) Logout(ctx context.Context, identifier string) error {
	if identifier == "" {
		return nil
	}
	return s.store.RevokeSession(ctx, hashIdentifier(identifier), s.now().UTC())
}
