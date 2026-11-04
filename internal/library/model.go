// Package library owns source configuration and folder-to-album mapping. Source
// files are read-only; only the database and independent derivative cache mutate.
package library

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrSourceSettings = errors.New("source settings or folder mapping are invalid")
	ErrUnknownRoot    = errors.New("select a declared source root from the runtime configuration")
	ErrRootInUse      = errors.New("the configured root is already registered to a source")
	ErrSourceNotEmpty = errors.New("a source with indexed albums or entries cannot be deleted; deactivate it instead")
	ErrSourceChanged  = errors.New("the source configuration changed or is no longer active and configured")
)

// Depths count directory segments from the root, independent of any historical
// NAS layout. AlbumDepth zero groups the entire source; PersonDepth zero disables
// person mapping. Shallower files stay in their actual parent rather than vanish.
type Mapping struct {
	AlbumDepth  int `json:"album_depth"`
	PersonDepth int `json:"person_depth"`
}

func DefaultMapping() Mapping { return Mapping{AlbumDepth: 1} }
func (mapping Mapping) Validate() error {
	if mapping.AlbumDepth < 0 || mapping.AlbumDepth > 16 || mapping.PersonDepth < 0 || mapping.PersonDepth > mapping.AlbumDepth {
		return ErrSourceSettings
	}
	return nil
}

type Location struct{ AlbumPath, AlbumName, PersonName string }

func (mapping Mapping) Locate(relative, rootName string) (Location, error) {
	if mapping.Validate() != nil || relative == "." || !fs.ValidPath(relative) || strings.ContainsAny(relative, "\\\x00") {
		return Location{}, ErrSourceSettings
	}
	segments := strings.Split(relative, "/")
	directories := segments[:len(segments)-1]
	depth := min(mapping.AlbumDepth, len(directories))
	location := Location{AlbumPath: ".", AlbumName: rootName}
	if depth > 0 {
		location.AlbumPath = strings.Join(directories[:depth], "/")
		location.AlbumName = path.Base(location.AlbumPath)
	}
	if mapping.PersonDepth > 0 && mapping.PersonDepth <= len(directories) {
		location.PersonName = directories[mapping.PersonDepth-1]
	}
	return location, nil
}

type SourceSettings struct {
	LibraryID               string  `json:"library_id"`
	RootName                string  `json:"root_name"`
	Mapping                 Mapping `json:"mapping"`
	Active                  bool    `json:"active"`
	MinimumFiles            int64   `json:"minimum_files"`
	PollIntervalSeconds     int64   `json:"poll_interval_seconds"`
	FullScanIntervalSeconds int64   `json:"full_scan_interval_seconds"`
}

// ValidRootName bounds a stable YAML/API reference to the shared cursor ID size.
// It is a label, never an API-supplied filesystem path.
func ValidRootName(name string) bool {
	return name != "" && len(name) <= 64 && utf8.ValidString(name) && strings.TrimSpace(name) == name && !strings.ContainsFunc(name, unicode.IsControl)
}

func DefaultSourceSettings() SourceSettings {
	return SourceSettings{Mapping: DefaultMapping(), Active: true, PollIntervalSeconds: 60, FullScanIntervalSeconds: 86400}
}

func (settings SourceSettings) Validate() error {
	if settings.LibraryID == "" || len(settings.LibraryID) > 64 || !ValidRootName(settings.RootName) || settings.Mapping.Validate() != nil || settings.MinimumFiles < 0 || settings.MinimumFiles > 1000000000 || settings.PollIntervalSeconds < 10 || settings.PollIntervalSeconds > 86400 || settings.FullScanIntervalSeconds < settings.PollIntervalSeconds || settings.FullScanIntervalSeconds > 604800 {
		return ErrSourceSettings
	}
	return nil
}

type SourcePatch struct {
	LibraryID               *string  `json:"library_id"`
	RootName                *string  `json:"root_name"`
	Mapping                 *Mapping `json:"mapping"`
	Active                  *bool    `json:"active"`
	MinimumFiles            *int64   `json:"minimum_files"`
	PollIntervalSeconds     *int64   `json:"poll_interval_seconds"`
	FullScanIntervalSeconds *int64   `json:"full_scan_interval_seconds"`
}

func (patch SourcePatch) apply(settings *SourceSettings) error {
	if patch.LibraryID == nil && patch.RootName == nil && patch.Mapping == nil && patch.Active == nil && patch.MinimumFiles == nil && patch.PollIntervalSeconds == nil && patch.FullScanIntervalSeconds == nil {
		return ErrSourceSettings
	}
	if patch.LibraryID != nil {
		settings.LibraryID = *patch.LibraryID
	}
	if patch.RootName != nil {
		settings.RootName = *patch.RootName
	}
	if patch.Mapping != nil {
		settings.Mapping = *patch.Mapping
	}
	if patch.Active != nil {
		settings.Active = *patch.Active
	}
	if patch.MinimumFiles != nil {
		settings.MinimumFiles = *patch.MinimumFiles
	}
	if patch.PollIntervalSeconds != nil {
		settings.PollIntervalSeconds = *patch.PollIntervalSeconds
	}
	if patch.FullScanIntervalSeconds != nil {
		settings.FullScanIntervalSeconds = *patch.FullScanIntervalSeconds
	}
	return settings.Validate()
}

type Source struct {
	ID string `json:"id"`
	SourceSettings
	Configured     bool       `json:"configured"`
	State          string     `json:"state"`
	ScanGeneration int64      `json:"scan_generation"`
	Version        int64      `json:"version"`
	ConfigVersion  int64      `json:"config_version"`
	LastCheckedAt  *time.Time `json:"last_checked_at"`
	LastHealthyAt  *time.Time `json:"last_healthy_at"`
	LastFullScanAt *time.Time `json:"last_full_scan_at"`
	LastErrorCode  *string    `json:"last_error_code"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	rootPath       string
	rootSignature  string
}

func (source Source) String() string { return fmt.Sprintf("source %s (%s)", source.ID, source.State) }

type Root struct {
	Name string `json:"root_name"`
}

const sourceSelect = `SELECT id,library_id,name,mapping_json,is_active,minimum_files,poll_interval_seconds,full_scan_interval_seconds,
 state,scan_generation,version,config_version,last_checked_at,last_healthy_at,last_full_scan_at,last_error_code,created_at,updated_at,root_path,root_config_sha256 FROM sources`

func scanSource(row interface{ Scan(...any) error }) (Source, error) {
	var source Source
	var mapping string
	var checked, healthy, full sql.NullInt64
	var created, updated int64
	err := row.Scan(&source.ID, &source.LibraryID, &source.RootName, &mapping, &source.Active, &source.MinimumFiles, &source.PollIntervalSeconds, &source.FullScanIntervalSeconds,
		&source.State, &source.ScanGeneration, &source.Version, &source.ConfigVersion, &checked, &healthy, &full, &source.LastErrorCode, &created, &updated, &source.rootPath, &source.rootSignature)
	if err != nil {
		return Source{}, err
	}
	source.Mapping = DefaultMapping()
	decoder := json.NewDecoder(strings.NewReader(mapping))
	decoder.DisallowUnknownFields()
	if !json.Valid([]byte(mapping)) || strings.TrimSpace(mapping) == "null" {
		return Source{}, errors.New("invalid stored source mapping")
	}
	if err := decoder.Decode(&source.Mapping); err != nil {
		return Source{}, err
	}
	if err := source.Mapping.Validate(); err != nil {
		return Source{}, err
	}
	source.CreatedAt, source.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	source.LastCheckedAt, source.LastHealthyAt, source.LastFullScanAt = sourceTime(checked), sourceTime(healthy), sourceTime(full)
	return source, nil
}

func sourceTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	instant := time.UnixMilli(value.Int64).UTC()
	return &instant
}
