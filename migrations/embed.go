package migrations

import "embed"

// FS holds all embedded SQL migration files in lexicographical order.
//
//go:embed *.sql
var FS embed.FS
