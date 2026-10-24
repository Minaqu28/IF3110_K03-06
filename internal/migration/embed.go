package migration

import "embed"

// Files contains the platform database schema migrations.
//go:embed *.sql
var Files embed.FS
