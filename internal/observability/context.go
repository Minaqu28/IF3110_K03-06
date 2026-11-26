package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type contextKey int

const (
	correlationIDKey contextKey = iota + 1
	actorKey
	databaseKey
)

type ActorInfo struct {
	ID       int64
	Username string
}

type DatabaseInfo struct {
	ID   int64
	Name string
}

func NewCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

func WithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationIDKey, id)
}

func CorrelationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(correlationIDKey).(string)
	return id
}

func WithActor(ctx context.Context, id int64, username string) context.Context {
	if id <= 0 && username == "" {
		return ctx
	}
	return context.WithValue(ctx, actorKey, ActorInfo{ID: id, Username: username})
}

func ActorFromContext(ctx context.Context) (ActorInfo, bool) {
	if ctx == nil {
		return ActorInfo{}, false
	}
	info, ok := ctx.Value(actorKey).(ActorInfo)
	return info, ok
}

func WithDatabase(ctx context.Context, id int64, name string) context.Context {
	if id <= 0 && name == "" {
		return ctx
	}
	return context.WithValue(ctx, databaseKey, DatabaseInfo{ID: id, Name: name})
}

func DatabaseFromContext(ctx context.Context) (DatabaseInfo, bool) {
	if ctx == nil {
		return DatabaseInfo{}, false
	}
	info, ok := ctx.Value(databaseKey).(DatabaseInfo)
	return info, ok
}
