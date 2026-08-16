// Package migrations embeds the schema so the deployed binary cannot disagree
// with the database it expects. It lives at the repo root rather than inside
// internal/store because go:embed cannot reach a parent directory.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
