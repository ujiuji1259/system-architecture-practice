// Package db bundles the SQL schema as embedded assets.
package db

import (
	_ "embed"
)

// Schema is the full database schema, applied on startup.
//
//go:embed migrations/0001_init.sql
var Schema string
