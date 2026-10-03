// Package migrations embeds the SQL migrations so the binary can migrate without the source tree.
package migrations

import "embed"

//go:embed postgres/*.sql
var Postgres embed.FS
