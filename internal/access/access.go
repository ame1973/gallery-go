// Package access owns group grants and the shared SQL visibility rules. Library
// access is mandatory; album restrictions only narrow it. Empty grants deny access.
package access

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gallerygo/internal/auth"
)

var (
	ErrName         = errors.New("name must contain 1 to 100 characters, at most 400 UTF-8 bytes, without control characters")
	ErrNameTaken    = errors.New("the group name is already in use")
	ErrGroupSet     = errors.New("provide an explicit array of at most 100 unique group IDs")
	ErrUnknownGroup = errors.New("one or more permission groups do not exist")
	ErrRestriction  = errors.New("unrestricted albums must have an empty group list")
)

type Service struct {
	db   *sql.DB
	auth *auth.Service
	now  func() time.Time
}

func New(db *sql.DB, authentication *auth.Service) *Service {
	return &Service{db: db, auth: authentication, now: time.Now}
}

func (s *Service) adminTx(ctx context.Context, current auth.Session) (*sql.Tx, auth.User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, auth.User{}, err
	}
	actor, err := s.auth.RequireAdmin(ctx, tx, current)
	if err != nil {
		tx.Rollback()
		return nil, auth.User{}, err
	}
	return tx, actor.User, nil
}

func (s *Service) readTx(ctx context.Context, current auth.Session) (*sql.Tx, auth.User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, auth.User{}, err
	}
	actor, err := s.auth.SessionInTx(ctx, tx, current)
	if err != nil {
		tx.Rollback()
		return nil, auth.User{}, err
	}
	return tx, actor.User, nil
}

func normalizedName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || len(name) > 400 || utf8.RuneCountInString(name) > 100 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrName
	}
	return name, nil
}

func pageLimit(limit int) bool { return limit >= 1 && limit <= 101 }
