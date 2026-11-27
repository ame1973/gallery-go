//go:build search_candidate_probe

package search

import (
	"context"
	"strings"
	"testing"
	"time"
)

func repeatedFalseDocument() string { return strings.Repeat("台", 1363) + "叝叙" }
func repeatedFalseQuery() string    { return strings.Repeat("台", 677) + "参叄参又叝叙" }

// This is a synthetic lower-bound probe, not an authenticated search endpoint.
func TestLiteralCandidates100KAdversarialProbe(t *testing.T) {
	db := candidateDB(t)
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE candidate_docs(id INTEGER PRIMARY KEY,normalized TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	falseDoc, err := EncodeLiteralDocument(repeatedFalseDocument())
	if err != nil {
		t.Fatal(err)
	}
	query := repeatedFalseQuery()
	trueDoc, err := EncodeLiteralDocument(query)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := CompileLiteralCandidates(query)
	if err != nil {
		t.Fatal(err)
	}
	if len(falseDoc.Normalized) != 4095 {
		t.Fatalf("unexpected adversarial document length: %d", len(falseDoc.Normalized))
	}
	if ok, err := plan.ContainsNormalized(falseDoc.Normalized); err != nil || ok {
		t.Fatalf("invalid adversarial false document: %t %v", ok, err)
	}

	started := time.Now()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	docStmt, err := tx.PrepareContext(t.Context(), `INSERT INTO candidate_docs(id,normalized) VALUES(?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer docStmt.Close()
	ftsStmt, err := tx.PrepareContext(t.Context(), `INSERT INTO candidate_fts(rowid,postings) VALUES(?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer ftsStmt.Close()
	const total = 100_000
	for id := 1; id <= total; id++ {
		doc := falseDoc
		if id == total {
			doc = trueDoc
		}
		if _, err := docStmt.ExecContext(t.Context(), id, doc.Normalized); err != nil {
			t.Fatalf("document %d: %v", id, err)
		}
		if _, err := ftsStmt.ExecContext(t.Context(), id, doc.Postings); err != nil {
			t.Fatalf("postings %d: %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("setup=%s rows=%d; setup is outside the query deadline", time.Since(started), total)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started = time.Now()
	rows, err := db.QueryContext(ctx, `SELECT d.id,d.normalized FROM candidate_fts JOIN candidate_docs d ON d.id=candidate_fts.rowid WHERE candidate_fts MATCH ? ORDER BY d.id`, plan.MatchExpression())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var scanned, matches, matchingID int
	for rows.Next() {
		var id int
		var normalized string
		if err := rows.Scan(&id, &normalized); err != nil {
			t.Fatal(err)
		}
		scanned++
		matched, err := plan.ContainsNormalized(normalized)
		if err != nil {
			t.Fatal(err)
		}
		if matched {
			matches++
			matchingID = id
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query failed after %s; scanned=%d matches=%d: %v", time.Since(started), scanned, matches, err)
	}
	if scanned != total || matches != 1 || matchingID != total {
		t.Fatalf("query after %s: scanned=%d matches=%d matchingID=%d", time.Since(started), scanned, matches, matchingID)
	}
	t.Logf("query=%s scanned=%d exact_matches=%d", time.Since(started), scanned, matches)
}
