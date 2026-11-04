package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var requestKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

type userRequest struct {
	actor, key, operation, metadata, passwordHash string
}

// Password equality for a retry uses bcrypt, not a fast fingerprint that would
// weaken password storage. Expensive work stays outside SQLite's writer lock.
func (s *Service) prepareUserRequest(ctx context.Context, actor Session, key, operation string, metadata any, password string) (userRequest, error) {
	if !requestKeyPattern.MatchString(key) {
		return userRequest{}, ErrIdempotencyKey
	}
	if err := validatePassword(password); err != nil {
		return userRequest{}, err
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return userRequest{}, err
	}
	request := userRequest{actor: actor.User.ID, key: key, operation: operation, metadata: string(data)}
	release, err := s.passwordSlot(ctx)
	if err != nil {
		return userRequest{}, err
	}
	defer release()
	var previousOperation, previousMetadata string
	err = s.db.QueryRowContext(ctx, `SELECT operation,request_json,password_hash FROM admin_user_requests WHERE actor_user_id=? AND request_key=? AND expires_at>?`, request.actor, key, s.now().UnixMilli()).Scan(&previousOperation, &previousMetadata, &request.passwordHash)
	if err == nil {
		if previousOperation != operation || previousMetadata != request.metadata || bcrypt.CompareHashAndPassword([]byte(request.passwordHash), []byte(password)) != nil {
			return userRequest{}, ErrIdempotencyReuse
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		request.passwordHash, err = HashPassword(password)
		if err != nil {
			return userRequest{}, err
		}
	} else {
		return userRequest{}, err
	}
	return request, nil
}

func (request userRequest) replay(ctx context.Context, tx *sql.Tx, now time.Time) (User, bool, error) {
	var operation, metadata, hash, result string
	err := tx.QueryRowContext(ctx, `SELECT operation,request_json,password_hash,result_json FROM admin_user_requests WHERE actor_user_id=? AND request_key=? AND expires_at>?`, request.actor, request.key, now.UnixMilli()).Scan(&operation, &metadata, &hash, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	if operation != request.operation || metadata != request.metadata {
		return User{}, false, ErrIdempotencyReuse
	}
	if hash != request.passwordHash {
		// A concurrent first request won after our lookup. Retry outside this
		// transaction so bcrypt never blocks other writers behind its comparison.
		return User{}, false, ErrRequestInFlight
	}
	var user User
	err = json.Unmarshal([]byte(result), &user)
	return user, true, err
}

func (request userRequest) save(ctx context.Context, tx *sql.Tx, user User, now time.Time) error {
	result, err := json.Marshal(user)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_user_requests WHERE expires_at<=?`, now.UnixMilli()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_user_requests(actor_user_id,request_key,operation,request_json,password_hash,result_json,expires_at) VALUES(?,?,?,?,?,?,?)`, request.actor, request.key, request.operation, request.metadata, request.passwordHash, string(result), now.Add(24*time.Hour).UnixMilli())
	return err
}
