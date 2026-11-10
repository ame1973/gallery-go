//go:build !linux

package media

import "io/fs"

// Other platforms retain the portable size/mtime checks; their change-time
// semantics are not assumed to match Linux.
func contentChangeTimeChanged(_, _ fs.FileInfo) bool { return false }
