// Package migrations embeds the SQL migrations so the binary and tests can
// apply them without a separate CLI. docker-compose mounts the same files.
package migrations

import "embed"

// FS holds the golang-migrate *.up.sql / *.down.sql files.
//
//go:embed *.sql
var FS embed.FS
