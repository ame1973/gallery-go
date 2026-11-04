package jobs

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"gallerygo/internal/store"
)

// Handler does blocking work outside SQLite transactions. It must observe ctx and
// use RequireLeaseTx/CompleteTx when publishing business changes. A handler that
// does not publish in its own transaction must report final counts before return.
type Handler func(ctx context.Context, lease Lease) error

// Failure exposes only a stable diagnostic code, never a subprocess stderr or
// private source path. The queue bounds automatic attempts even when retryable.
type Failure struct {
	Code      string
	Retryable bool
}

func (failure *Failure) Error() string {
	if failure != nil && failureMessages[failure.Code] != "" {
		return failureMessages[failure.Code]
	}
	return failureMessages["INTERNAL_ERROR"]
}

// Run shares the same claim/lease machinery for embedded and standalone workers.
// Cancellation stops new claims, cancels active handlers, and safely requeues live
// owned work; expired or replaced owners never alter the newer attempt.
func (q *Queue) Run(ctx context.Context, role string, handlers map[string]Handler) error {
	if (role != "serve" && role != "worker") || len(handlers) == 0 || len(handlers) > 32 {
		return ErrInvalidJob
	}
	handlers = maps.Clone(handlers)
	kinds := make([]string, 0, len(handlers))
	for kind, handler := range handlers {
		if !kindPattern.MatchString(kind) || handler == nil {
			return ErrInvalidJob
		}
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	id := store.NewID()
	if err := q.RegisterWorker(ctx, id, role, q.policy.Concurrency); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	results := make(chan error, q.policy.Concurrency)
	for range q.policy.Concurrency {
		go func() { results <- q.runSlot(runCtx, id, kinds, handlers) }()
	}
	heartbeat := time.NewTicker(q.policy.Heartbeat)
	defer heartbeat.Stop()
	stopping := runCtx.Done()
	var failure error
	for remaining := q.policy.Concurrency; remaining > 0; {
		select {
		case err := <-results:
			remaining--
			if err != nil && failure == nil {
				failure = err
				cancel(err)
			}
		case <-heartbeat.C:
			if runCtx.Err() == nil {
				if err := q.WorkerHeartbeat(runCtx, id); err != nil && runCtx.Err() == nil {
					failure = err
					cancel(err)
				}
			}
		case <-stopping:
			heartbeat.Stop()
			stopping = nil
		}
	}
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	return errors.Join(failure, q.StopWorker(cleanup, id))
}

func (q *Queue) runSlot(ctx context.Context, id string, kinds []string, handlers map[string]Handler) error {
	for ctx.Err() == nil {
		lease, err := q.Claim(ctx, id, kinds)
		if errors.Is(err, ErrNoWork) {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(q.policy.Poll):
				continue
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := q.execute(ctx, lease, handlers[lease.Job.Kind]); err != nil {
			return err
		}
	}
	return nil
}

func (q *Queue) execute(parent context.Context, lease Lease, handler Handler) error {
	ctx, cancel := context.WithCancelCause(parent)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(q.policy.Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := q.Heartbeat(ctx, lease); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	err := handler(ctx, lease)
	cancel(err)
	<-heartbeatDone
	cause := context.Cause(ctx)
	settle, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer stop()
	var result error
	switch {
	case errors.Is(cause, ErrLeaseLost):
		return nil
	case errors.Is(cause, ErrCancelled):
		result = q.AcknowledgeCancel(settle, lease)
	case parent.Err() != nil:
		result = q.Fail(settle, lease, "WORKER_STOPPED", true)
	case err == nil && errors.Is(cause, context.Canceled):
		result = q.Complete(settle, lease)
		if errors.Is(result, ErrProgress) {
			// Missing/inconsistent final progress is a handler defect, not success.
			result = q.Fail(settle, lease, "INTERNAL_ERROR", false)
		}
	default:
		code, retryable := "INTERNAL_ERROR", true
		var failure *Failure
		if errors.As(err, &failure) && failure != nil && failureMessages[failure.Code] != "" {
			code, retryable = failure.Code, failure.Retryable
		}
		result = q.Fail(settle, lease, code, retryable)
	}
	if errors.Is(result, ErrCancelled) {
		result = q.AcknowledgeCancel(settle, lease)
	}
	if errors.Is(result, ErrLeaseLost) {
		return nil // Already completed atomically, expired, or replaced; never rewrite.
	}
	return result
}
