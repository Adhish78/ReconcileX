// Package migrations exposes all SQL schema migration files embedded directly
// into the compiled Go binary.
//
// Embedding migrations avoids shipping separate .sql files or external migration
// tools in the runtime container, allowing cmd/reconcilex and internal/api to
// apply and verify the schema directly from memory.
package migrations

import "embed"

// FS holds every *.sql migration file in this directory as a read-only embedded
// filesystem. internal/db consumes this FS in lexicographical filename order
// (e.g., 0001_users_and_identities.sql, 0002_...).
//
//go:embed *.sql
var FS embed.FS
