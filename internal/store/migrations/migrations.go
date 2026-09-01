package migrations

import "embed"

// Files contains the ordered SQL migrations bundled with the service.
//
//go:embed *.sql
var Files embed.FS
