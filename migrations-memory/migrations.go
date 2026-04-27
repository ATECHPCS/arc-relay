// Package migrationsmemory holds DDL for the centralized transcript memory
// database, opened as a separate SQLite file from the relay's main DB.
package migrationsmemory

import "embed"

//go:embed *.sql
var FS embed.FS
