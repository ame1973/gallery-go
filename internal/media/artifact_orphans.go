package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"

	"gallerygo/internal/jobs"
)

// A private single known revision/preset cohort, never a recursive cache sweep or
// public raw-SHA request. Pins are captured from indexed metadata before enqueue.
type artifactOrphanRequest struct {
	MediaID         string `json:"media_id"`
	SourceID        string `json:"source_id"`
	RevisionID      string `json:"revision_id"`
	RevisionVersion int64  `json:"revision_version"`
	SourceSHA256    string `json:"source_sha256"`
	SourceSize      int64  `json:"source_size"`
	SourceMTime     int64  `json:"source_mtime_ns"`
	Preset          string `json:"preset"`
	EntryLimit      int    `json:"entry_limit"`
	ByteLimit       int64  `json:"byte_limit"`
}

func (s *Service) artifactOrphanInput(request artifactOrphanRequest) (jobs.Input, error) {
	if !artifactTempJobID(request.SourceID) || !filepath.IsAbs(s.cacheRoot) || filepath.Clean(s.cacheRoot) != s.cacheRoot || request.RevisionVersion < 1 || !validHash(request.SourceSHA256) || request.SourceSize < 1 || request.SourceSize > 1<<40 || request.EntryLimit < 1 || request.EntryLimit > 1000 || request.ByteLimit < 1 || request.ByteLimit > 64<<20 {
		return jobs.Input{}, errArtifactTemps
	}
	if _, err := stillInputWorkKey(request.MediaID, request.RevisionID, request.Preset); err != nil {
		return jobs.Input{}, errArtifactTemps
	}
	payload, _ := json.Marshal(struct {
		Version int                   `json:"version"`
		Request artifactOrphanRequest `json:"request"`
	}{1, request})
	identity, _ := json.Marshal(struct {
		Cache   string
		Payload json.RawMessage
	}{s.cacheRoot, payload})
	digest := sha256.Sum256(identity)
	key := "artifact_orphan:" + hex.EncodeToString(digest[:])
	return jobs.Input{Kind: "reconcile_artifact", ResourceKey: key, Dedupe: key, Payload: payload}, nil
}

type artifactOrphanReport struct {
	entries, bytes, unlinked, retained int64
	limited                            bool
}

func (report artifactOrphanReport) String() string { return "private immutable-artifact report" }
