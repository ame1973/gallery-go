package media

import (
	"os"
	"testing"

	"gallerygo/internal/sandbox"
)

// Imported probe calls self-exec this test image, not a configurable executable.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == sandbox.ChildCommand {
		os.Exit(sandbox.RunChild(os.Args[2:]))
	}
	os.Exit(m.Run())
}
