package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var (
	ErrIdempotencyKey   = errors.New("an idempotency key of 16 to 128 ASCII letters, digits, underscores, or hyphens is required")
	ErrIdempotencyReuse = errors.New("the idempotency key was used for a different request")
	ErrRequestInFlight  = errors.New("a concurrent request completed; retry with the same idempotency key")
	requestKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
)

// Command is a private, transaction-bound POST receipt, not an authorization
// capability. Metadata must not contain passwords; PasswordHash is either empty
// or an already computed bcrypt hash, never a fast fingerprint or raw password.
// Callers must reauthorize the actor before Replay and commit Save with the write.
type Command struct {
	Actor, Key, Operation, Metadata, PasswordHash string
}

func ValidRequestKey(key string) bool { return requestKeyPattern.MatchString(key) }

func (request Command) Replay(ctx context.Context, tx *sql.Tx, now time.Time, result any) (bool, error) {
	if !ValidRequestKey(request.Key) {
		return false, ErrIdempotencyKey
	}
	var operation, metadata, hash, data string
	err := tx.QueryRowContext(ctx, `SELECT operation,request_json,password_hash,result_json FROM mutation_receipts WHERE actor_user_id=? AND request_key=? AND expires_at>?`, request.Actor, request.Key, now.UnixMilli()).Scan(&operation, &metadata, &hash, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if operation != request.Operation || metadata != request.Metadata {
		return false, ErrIdempotencyReuse
	}
	if hash != request.PasswordHash {
		// A concurrent password-bearing request won after the caller's lookup.
		// Retry outside the writer lock so bcrypt cannot block other mutations.
		return false, ErrRequestInFlight
	}
	return true, json.Unmarshal([]byte(data), result)
}

func (request Command) Save(ctx context.Context, tx *sql.Tx, result any, now time.Time) error {
	if !ValidRequestKey(request.Key) {
		return ErrIdempotencyKey
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mutation_receipts WHERE expires_at<=?`, now.UnixMilli()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mutation_receipts(actor_user_id,request_key,operation,request_json,password_hash,result_json,expires_at) VALUES(?,?,?,?,?,?,?)`, request.Actor, request.Key, request.Operation, request.Metadata, request.PasswordHash, string(data), now.Add(24*time.Hour).UnixMilli())
	return err
}
