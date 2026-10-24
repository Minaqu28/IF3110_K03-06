package frontend

import "embed"

// Files contains the pages and static assets served by the application.
//go:embed pages/*.html assets/*
var Files embed.FS
