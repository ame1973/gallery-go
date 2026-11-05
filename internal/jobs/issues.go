package jobs

import (
	"context"
	"io/fs"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gallerygo/internal/auth"
)

// Issue is a committed historical diagnostic, never a raw error, source-root
// path, worker capability or file-opening token. A null path is not guessed.
// Counts remain cumulative work units and cannot be inferred from issue count.
type Issue struct {
	ID                  string    `json:"id"`
	Attempt             int       `json:"attempt"`
	SourceID            string    `json:"source_id"`
	SourceConfigVersion int64     `json:"source_config_version"`
	GenerationID        *string   `json:"generation_id"`
	RelativePath        *string   `json:"relative_path"`
	ErrorCode           string    `json:"error_code"`
	Message             string    `json:"message"`
	CreatedAt           time.Time `json:"created_at"`
}

func (a *Admin) Issues(ctx context.Context, current auth.Session, id, after string, limit int) ([]Issue, error) {
	if limit < 1 || limit > 101 {
		return nil, ErrInvalidJob
	}
	tx, err := a.queue.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := a.auth.RequireAdmin(ctx, tx, current); err != nil {
		return nil, err
	}
	var exists string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id=?`, id).Scan(&exists); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,attempt,source_id,source_config_version,NULLIF(generation_id,''),relative_path,error_code,created_at FROM job_issues WHERE job_id=? AND id>? ORDER BY id LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Issue, 0)
	for rows.Next() {
		var item Issue
		var path, code string
		var created int64
		if err := rows.Scan(&item.ID, &item.Attempt, &item.SourceID, &item.SourceConfigVersion, &item.GenerationID, &path, &code, &created); err != nil {
			return nil, err
		}
		item.RelativePath = issuePath(path)
		item.ErrorCode, item.Message = issueDescription(code)
		item.CreatedAt = time.UnixMilli(created).UTC()
		items = append(items, item)
	}
	return items, rows.Err()
}

func issuePath(value string) *string {
	if len(value) > 4096 || !utf8.ValidString(value) || !fs.ValidPath(value) || strings.ContainsAny(value, "\\\x00") {
		return nil
	}
	if len(value) >= 3 && value[1] == ':' && value[2] == '/' && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) {
		return nil
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			return nil
		}
	}
	return &value
}

var issueMessages = map[string]string{
	"SOURCE_NOT_CONFIGURED":       "The source is not configured in this process.",
	"MINIMUM_FILES":               "The inventory did not meet the source's minimum-file guard.",
	"SCAN_LIMIT":                  "The inventory exceeded a configured safety limit.",
	"UNSAFE_ENTRY":                "The entry could not be handled within the source boundary.",
	"SYMLINK_CYCLE":               "A directory symlink cycle was detected.",
	"MEDIA_LIMIT":                 "The input exceeded a configured metadata resource limit.",
	"CODEC_UNAVAILABLE":           "No supported isolated metadata capability is available for the input.",
	"AMBIGUOUS_RENAME":            "Multiple possible renamed entries matched; no identity was guessed.",
	"AMBIGUOUS_ALBUM_RENAME":      "The folder move could not be identified unambiguously.",
	"TARGET_RENAME_OUTSIDE_SCOPE": "The possible move crosses the selected scan scope and was deferred.",
}

func issueDescription(code string) (string, string) {
	if message, ok := issueMessages[code]; ok {
		return code, message
	}
	if message, ok := failureMessages[code]; ok {
		return code, message
	}
	return "PROCESSING_FAILED", failureMessages["PROCESSING_FAILED"]
}
