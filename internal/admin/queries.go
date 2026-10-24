package admin

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

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
