package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"gallerygo/internal/jobs"
)

var (
	errArtifactTemps = errors.New("private temporary-artifact reconciliation is unavailable")
	artifactTempName = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.tmp$`)
)

// This is a private bounded batch, not a public full-cache cleanup request.
type artifactTempRequest struct {
	AfterJobID string `json:"after_job_id"`
	JobLimit   int    `json:"job_limit"`
	EntryLimit int    `json:"entry_limit"`
}

func artifactTempJobID(id string) bool {
	return localPath(id) && !strings.ContainsAny(id, "/\x00\r\n") && len(id) <= 128
}

func (s *Service) artifactTempInput(request artifactTempRequest) (jobs.Input, error) {
	if !filepath.IsAbs(s.cacheRoot) || filepath.Clean(s.cacheRoot) != s.cacheRoot || request.JobLimit < 1 || request.JobLimit > 100 || request.EntryLimit < 1 || request.EntryLimit > 1000 || (request.AfterJobID != "" && !artifactTempJobID(request.AfterJobID)) {
		return jobs.Input{}, errArtifactTemps
	}
	payload, _ := json.Marshal(struct {
		Version int                 `json:"version"`
		Request artifactTempRequest `json:"request"`
	}{1, request})
	identity, _ := json.Marshal(struct {
		Cache   string
		Payload json.RawMessage
	}{s.cacheRoot, payload})
	digest := sha256.Sum256(identity)
	key := "artifact_temps:" + hex.EncodeToString(digest[:])
	return jobs.Input{Kind: "reconcile_temps", ResourceKey: key, Dedupe: key, Payload: payload}, nil
}

// Job scopes are work units; unlinked files are separate physical observations.
// All fields stay private. Audit receives only explicitly selected safe counters.
type artifactTempReport struct {
	scopes, entries, unlinked, retained int64
	nextJobID                           string
	hasMore, limited                    bool
}

func (report artifactTempReport) String() string { return "private temporary-artifact report" }
