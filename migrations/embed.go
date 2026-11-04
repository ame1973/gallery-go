// Package migrations contains the immutable, ordered database migrations.
package migrations

import "embed"

// Files is embedded so migrations do not depend on the process working directory.
//
//go:embed *.sql
var Files embed.FS
