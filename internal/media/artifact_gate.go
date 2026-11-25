package media

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

// artifactGate serializes cooperating cache writers/publication against future
// orphan reconciliation. It is not a job lease or a rendered-pixel capability.
// Never pass its descriptor to a codec child or unlink its persistent lock file.
type artifactGate struct {
	root               *os.Root
	file               *os.File
	rootInfo, lockInfo fs.FileInfo
	cacheRoot          string
	mountGuard         func(context.Context) error
	mountSources       []*os.File
	exclusive          bool
}

func (gate *artifactGate) String() string { return "private artifact gate" }
func (gate *artifactGate) Close() error {
	if gate == nil {
		return nil
	}
	var fileErr, sourceErr, rootErr error
	if gate.file != nil {
		fileErr = gate.file.Close()
		gate.file = nil
	}
	for _, source := range gate.mountSources {
		sourceErr = errors.Join(sourceErr, source.Close())
	}
	gate.mountSources = nil
	if gate.root != nil {
		rootErr = gate.root.Close()
		gate.root = nil
	}
	return errors.Join(fileErr, sourceErr, rootErr)
}

// storedStillArtifact keeps the shared gate through a future publisher's final
// source/revision/live-lease-fenced transaction. Close after publication or failure,
// never before it. Encoded storage evidence still cannot prove decoded pixels or
// publish READY. All fields are private and have no serialized representation.
type storedStillArtifact struct {
	artifact stillArtifact
	jobID    string
	gate     *artifactGate
}

func (stored *storedStillArtifact) String() string { return "stored private artifact" }
func (stored *storedStillArtifact) Close() error {
	if stored == nil {
		return nil
	}
	return stored.gate.Close()
}
