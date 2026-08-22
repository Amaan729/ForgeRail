// Package migrations embeds the SQL schema files so the binary can migrate
// its own database on startup.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
