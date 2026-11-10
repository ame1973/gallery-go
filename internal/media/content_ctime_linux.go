//go:build linux

package media

import (
	"io/fs"
	"syscall"
)

// ctime detects common in-place rewrites that restore both size and mtime.
// It is a change signal, not a substitute for rehashing the whole source.
func contentChangeTimeChanged(before, after fs.FileInfo) bool {
	old, oldOK := before.Sys().(*syscall.Stat_t)
	current, currentOK := after.Sys().(*syscall.Stat_t)
	return !oldOK || !currentOK || old.Ctim != current.Ctim
}
