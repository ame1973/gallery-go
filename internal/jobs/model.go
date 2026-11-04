// Package jobs implements a durable SQLite work queue with expiring fenced
// leases. Workers do I/O outside write transactions and recheck the lease before
// publishing business changes. Private payloads and lease owners never enter DTOs.
package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

var (
	ErrInvalidJob   = errors.New("invalid job kind, resource, payload, or retry policy")
	ErrNoWork       = errors.New("no due work is available")
	ErrLeaseLost    = errors.New("the job lease is expired or no longer owned by this attempt")
	ErrCancelled    = errors.New("job cancellation was requested")
	ErrProgress     = errors.New("job counters must be nonnegative, consistent, and monotonic")
	ErrState        = errors.New("the job cannot transition from its current state")
	ErrResourceBusy = errors.New("another active job owns the resource deduplication key")
	kindPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	keyPattern      = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,256}$`)
)

type Policy struct {
	Concurrency int           `yaml:"concurrency"`
	Lease       time.Duration `yaml:"lease"`
	Heartbeat   time.Duration `yaml:"heartbeat"`
	Poll        time.Duration `yaml:"poll"`
	MaxAttempts int           `yaml:"max_attempts"`
	RetryBase   time.Duration `yaml:"retry_base"`
	RetryMax    time.Duration `yaml:"retry_max"`
}

func DefaultPolicy() Policy {
	return Policy{Concurrency: 2, Lease: 30 * time.Second, Heartbeat: 5 * time.Second, Poll: 500 * time.Millisecond, MaxAttempts: 3, RetryBase: time.Second, RetryMax: 5 * time.Minute}
}

func (p Policy) Validate() error {
	if p.Concurrency < 1 || p.Concurrency > 16 || p.Lease < 3*time.Second || p.Lease > 10*time.Minute || p.Heartbeat < 500*time.Millisecond || p.Heartbeat > p.Lease/3 || p.Poll < 100*time.Millisecond || p.Poll > 5*time.Second || p.MaxAttempts < 1 || p.MaxAttempts > 10 || p.RetryBase < 100*time.Millisecond || p.RetryBase > time.Minute || p.RetryMax < p.RetryBase || p.RetryMax > time.Hour {
		return ErrInvalidJob
	}
	return nil
}

// Counts are cumulative work units across attempts, not a claim that Done means
// all items succeeded. Attempt history also records the delta for each attempt.
// Retrying a job never resets these counters or erases previous failures.
type Counts struct {
	Total     int64 `json:"total"`
	Done      int64 `json:"done"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	Skipped   int64 `json:"skipped"`
	Cancelled int64 `json:"cancelled"`
}

func (c Counts) valid() bool {
	if c.Total < 0 || c.Done < 0 || c.Total < c.Done {
		return false
	}
	var sum int64
	for _, value := range []int64{c.Succeeded, c.Failed, c.Skipped, c.Cancelled} {
		if value < 0 || value > math.MaxInt64-sum {
			return false
		}
		sum += value
	}
	return sum == c.Done
}

func (c Counts) follows(previous Counts) bool {
	return c.valid() && c.Total >= previous.Total && c.Done >= previous.Done && c.Succeeded >= previous.Succeeded && c.Failed >= previous.Failed && c.Skipped >= previous.Skipped && c.Cancelled >= previous.Cancelled
}

func (c Counts) delta(base Counts) Counts {
	return Counts{c.Total - base.Total, c.Done - base.Done, c.Succeeded - base.Succeeded, c.Failed - base.Failed, c.Skipped - base.Skipped, c.Cancelled - base.Cancelled}
}

type Job struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	ResourceKey     string    `json:"resource_key"`
	Status          string    `json:"status"`
	Priority        int       `json:"priority"`
	CancelRequested bool      `json:"cancel_requested"`
	Attempts        int       `json:"attempts"`
	MaxAttempts     int       `json:"max_attempts"`
	NextRunAt       time.Time `json:"next_run_at"`
	Counts
	TotalKnown bool            `json:"total_known"`
	ErrorCode  *string         `json:"error_code"`
	LastError  *string         `json:"last_error"`
	Version    int64           `json:"version"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	StartedAt  *time.Time      `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	Payload    json.RawMessage `json:"-"`
	Dedupe     string          `json:"-"`
	leaseOwner string
	leaseUntil int64
}

func (job Job) String() string { return fmt.Sprintf("job %s (%s, %s)", job.ID, job.Kind, job.Status) }

type Lease struct {
	Job   Job    `json:"-"`
	Owner string `json:"-"`
}

func (lease Lease) String() string { return "lease for " + lease.Job.ID }

type Attempt struct {
	Number     int        `json:"attempt"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	ErrorCode  *string    `json:"error_code"`
	Message    *string    `json:"message"`
	Counts
}

const jobSelect = `SELECT id,kind,COALESCE(resource_key,''),status,priority,cancel_requested,attempts,max_attempts,next_run_at,
 total,done,succeeded,failed,skipped,cancelled,total_known,error_code,last_error,version,created_at,updated_at,started_at,finished_at,
 payload_json,dedupe_token,COALESCE(lease_owner,''),COALESCE(lease_expires_at,0) FROM jobs`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var job Job
	var next, created, updated int64
	var started, finished sql.NullInt64
	var payload string
	err := row.Scan(&job.ID, &job.Kind, &job.ResourceKey, &job.Status, &job.Priority, &job.CancelRequested, &job.Attempts, &job.MaxAttempts, &next,
		&job.Total, &job.Done, &job.Succeeded, &job.Failed, &job.Skipped, &job.Cancelled, &job.TotalKnown, &job.ErrorCode, &job.LastError, &job.Version,
		&created, &updated, &started, &finished, &payload, &job.Dedupe, &job.leaseOwner, &job.leaseUntil)
	job.NextRunAt, job.CreatedAt, job.UpdatedAt = time.UnixMilli(next).UTC(), time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	job.StartedAt, job.FinishedAt = optionalTime(started), optionalTime(finished)
	job.Payload = json.RawMessage(payload)
	return job, err
}

func optionalTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	timestamp := time.UnixMilli(value.Int64).UTC()
	return &timestamp
}

var failureMessages = map[string]string{
	"LEASE_EXPIRED":      "The previous worker's lease expired before completion.",
	"SOURCE_UNAVAILABLE": "The configured source or mount marker is unavailable.",
	"SOURCE_CHANGED":     "The source changed while the operation was running.",
	"IO_ERROR":           "A filesystem operation could not be completed safely.",
	"INVALID_MEDIA":      "The input media could not be validated.",
	"PROCESSING_FAILED":  "Media processing did not complete successfully.",
	"JOB_TIMEOUT":        "The operation exceeded its time limit.",
	"UNSUPPORTED_JOB":    "No compatible handler is available for this job.",
	"INTERNAL_ERROR":     "The operation could not be completed.",
}
