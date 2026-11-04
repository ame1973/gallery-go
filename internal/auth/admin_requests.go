package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"gallerygo/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// Password equality for a retry uses bcrypt, not a fast fingerprint that would
// weaken password storage. Expensive work stays outside SQLite's writer lock.
func (s *Service) prepareUserRequest(ctx context.Context, actor Session, key, operation string, metadata any, password string) (store.Command, error) {
	if !store.ValidRequestKey(key) {
		return store.Command{}, ErrIdempotencyKey
	}
	if err := validatePassword(password); err != nil {
		return store.Command{}, err
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return store.Command{}, err
	}
	request := store.Command{Actor: actor.User.ID, Key: key, Operation: operation, Metadata: string(data)}
	release, err := s.passwordSlot(ctx)
	if err != nil {
		return store.Command{}, err
	}
	defer release()
	var previousOperation, previousMetadata string
	err = s.db.QueryRowContext(ctx, `SELECT operation,request_json,password_hash FROM mutation_receipts WHERE actor_user_id=? AND request_key=? AND expires_at>?`, request.Actor, key, s.now().UnixMilli()).Scan(&previousOperation, &previousMetadata, &request.PasswordHash)
	if err == nil {
		if previousOperation != operation || previousMetadata != request.Metadata || bcrypt.CompareHashAndPassword([]byte(request.PasswordHash), []byte(password)) != nil {
			return store.Command{}, ErrIdempotencyReuse
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		request.PasswordHash, err = HashPassword(password)
		if err != nil {
			return store.Command{}, err
		}
	} else {
		return store.Command{}, err
	}
	return request, nil
}
