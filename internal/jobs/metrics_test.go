package jobs

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/auth"
)

func TestAdminMetricsSnapshotAndFreshAuthority(t *testing.T) {
	q, _ := fixture(t)
	admin, owner := adminFixture(t, q)
	job := enqueued(t, q, "metrics-snapshot", 0)

	// Open both connections before taking the writer lock, so pool creation is
	// not mistaken for a limitation of the existing WAL read transaction.
	first, err := q.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.db.Conn(t.Context())
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(t.Context(), `UPDATE jobs SET status='running' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	metrics, err := admin.Metrics(ctx, owner)
	cancel()
	if err != nil || metrics.Jobs != (StatusCounts{Queued: 1}) {
		t.Fatalf("reader under held writer: metrics=%+v err=%v", metrics, err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	metrics, err = admin.Metrics(t.Context(), owner)
	if err != nil || metrics.Jobs != (StatusCounts{Running: 1}) {
		t.Fatalf("committed status: metrics=%+v err=%v", metrics, err)
	}

	// The authorization read establishes the same snapshot used for the counts.
	snapshot, err := q.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback()
	if _, err := admin.auth.RequireAdmin(t.Context(), snapshot, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := q.db.ExecContext(t.Context(), `UPDATE jobs SET status='failed' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	counts, err := statusCounts(t.Context(), snapshot)
	if err != nil || counts != (StatusCounts{Running: 1}) {
		t.Fatalf("snapshot mixed versions: counts=%+v err=%v", counts, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := statusCounts(cancelled, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query: %v", err)
	}
	if err := snapshot.Rollback(); err != nil {
		t.Fatal(err)
	}
	forged := owner
	forged.User.ID, forged.User.Role = "not-the-real-user", "user"
	metrics, err = admin.Metrics(t.Context(), forged)
	if err != nil || metrics.Jobs != (StatusCounts{Failed: 1}) {
		t.Fatalf("trusted stale user DTO rather than session: metrics=%+v err=%v", metrics, err)
	}
	if _, err := admin.Metrics(cancelled, owner); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
	forged.Token = strings.Repeat("x", 43)
	if _, err := admin.Metrics(t.Context(), forged); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("forged session: %v", err)
	}
	if _, err := q.db.Exec(`UPDATE users SET role='user' WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Metrics(t.Context(), owner); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("demoted administrator: %v", err)
	}
	if _, err := q.db.Exec(`UPDATE users SET active=0 WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Metrics(t.Context(), owner); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("disabled account: %v", err)
	}
	if _, err := q.db.Exec(`UPDATE users SET active=1,role='admin' WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	authorized, err := q.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer authorized.Rollback()
	if _, err := admin.auth.RequireAdmin(t.Context(), authorized, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := q.db.Exec(`UPDATE sessions SET revoked_at=? WHERE id=?`, time.Now().UnixMilli(), owner.ID); err != nil {
		t.Fatal(err)
	}
	counts, err = statusCounts(t.Context(), authorized)
	if err != nil || counts != (StatusCounts{Failed: 1}) {
		t.Fatalf("revocation changed an existing snapshot: counts=%+v err=%v", counts, err)
	}
	if err := authorized.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Metrics(t.Context(), owner); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked session: %v", err)
	}
}

func TestAdminMetricsCancelsWaitingForConnection(t *testing.T) {
	q, _ := fixture(t)
	admin, owner := adminFixture(t, q)
	var held []*sql.Conn
	t.Cleanup(func() {
		for _, conn := range held {
			conn.Close()
		}
	})
	for range 4 {
		conn, err := q.db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	before := q.db.Stats().WaitCount
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := admin.Metrics(ctx, owner)
		result <- err
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for q.db.Stats().WaitCount == before {
		select {
		case err := <-result:
			t.Fatalf("metrics returned before the pool was released: %v", err)
		case <-deadline.C:
			t.Fatal("metrics did not wait for a connection")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting request did not cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request did not return")
	}
	if err := held[0].Close(); err != nil {
		t.Fatal(err)
	}
	metrics, err := admin.Metrics(t.Context(), owner)
	if err != nil || metrics.Jobs != (StatusCounts{}) {
		t.Fatalf("pool did not recover: metrics=%+v err=%v", metrics, err)
	}
}

func TestAdminMetrics100KUsesCoveringIndex(t *testing.T) {
	q, _ := fixture(t)
	admin, owner := adminFixture(t, q)
	_, err := q.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (
		VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000
	) INSERT INTO jobs(id,kind,payload_json,status,next_run_at,created_at,updated_at)
	SELECT printf('metrics-%06d',x),'scan','{"private":"not-exported"}',
		CASE (x-1)%6 WHEN 0 THEN 'queued' WHEN 1 THEN 'running'
		WHEN 2 THEN 'succeeded' WHEN 3 THEN 'failed' WHEN 4 THEN 'cancelled'
		ELSE 'dead' END,0,0,0 FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+statusCountsQuery)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "COVERING INDEX idx_jobs_claim") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("status counts lost covering index: %s", joined)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	metrics, err := admin.Metrics(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	want := StatusCounts{Queued: 16667, Running: 16667, Succeeded: 16667, Failed: 16667, Cancelled: 16666, Dead: 16666}
	if metrics.Jobs != want {
		t.Fatalf("100k metrics = %+v, want %+v", metrics.Jobs, want)
	}
	t.Logf("100,000 indexed job rows counted in %s (local host)", time.Since(start))
}
