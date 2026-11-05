package scanner

import (
	"os"
	"testing"

	"gallerygo/internal/sandbox"
)

// The real helper dispatch prevents recursively running tests during inspection.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == sandbox.ChildCommand {
		os.Exit(sandbox.RunChild(os.Args[2:]))
	}
	os.Exit(m.Run())
}
