package search

import (
	"database/sql"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gallerygo/internal/store"
)

func candidateDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "candidates.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(t.Context(), `CREATE VIRTUAL TABLE candidate_fts USING fts5(postings,tokenize='unicode61')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func candidateRows(t *testing.T, db *sql.DB, expression string) map[int64]bool {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT rowid FROM candidate_fts WHERE candidate_fts MATCH ?`, expression)
	if err != nil {
		t.Fatalf("candidate MATCH: %v", err)
	}
	defer rows.Close()
	found := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

func TestCandidateEncodingAndBoundaries(t *testing.T) {
	doc, err := EncodeLiteralDocument("台/北")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Normalized != "台/北" || doc.Postings != "ue58fb0 u2f be58fb02f ue58c97 b2fe58c97" {
		t.Fatalf("unexpected document encoding: %+v", doc)
	}
	for _, sample := range []struct{ query, expression string }{
		{"台", `"ue58fb0"`},
		{"台/", `"be58fb02f"`},
		{"台/北", `"be58fb02f" AND "b2fe58c97"`},
		{"aaaa", `"b6161"`},
	} {
		plan, err := CompileLiteralCandidates(sample.query)
		if err != nil || plan.MatchExpression() != sample.expression {
			t.Fatalf("query %q: got %q, want %q, error %v", sample.query, plan.MatchExpression(), sample.expression, err)
		}
	}
	empty, err := EncodeLiteralDocument("")
	if err != nil || empty.Normalized != "" || empty.Postings != "" {
		t.Fatalf("empty document: %+v %v", empty, err)
	}
	for _, invalid := range []string{"\x00", string([]byte{0xff})} {
		if _, err := EncodeLiteralDocument(invalid); !errors.Is(err, ErrTextInvalid) {
			t.Fatalf("invalid document error: %v", err)
		}
		if _, err := CompileLiteralCandidates(invalid); !errors.Is(err, ErrTextInvalid) {
			t.Fatalf("invalid query error: %v", err)
		}
	}
	for _, invalid := range []string{"", "  "} {
		if _, err := CompileLiteralCandidates(invalid); !errors.Is(err, ErrTextInvalid) {
			t.Fatalf("empty query error: %v", err)
		}
	}
	if _, err := EncodeLiteralDocument(strings.Repeat("x", MaxInputBytes+1)); !errors.Is(err, ErrTextLimit) {
		t.Fatalf("oversized document error: %v", err)
	}
	if _, err := CompileLiteralCandidates(strings.Repeat("x", MaxInputBytes+1)); !errors.Is(err, ErrTextLimit) {
		t.Fatalf("oversized query error: %v", err)
	}
	if matched, err := (LiteralCandidates{}).ContainsNormalized("anything"); matched || !errors.Is(err, ErrTextInvalid) {
		t.Fatalf("zero candidate plan: matched=%t error=%v", matched, err)
	}
}

func TestCandidateFTSIncludesEveryLiteralMatch(t *testing.T) {
	db := candidateDB(t)
	documents := []string{
		"", "台北 / 東京", "Straße #1", "foo bar", "alpha", "bravo", "甲乙甲", "甲/乙",
		"🧡彩", `key AND OR NOT " ( * )`, "a x y b", strings.Repeat("㌖", 1365),
	}
	queries := []string{
		"台", "台北", "北 / 東", "STRASSE", "#1", "甲乙", "甲/乙", "甲乙甲",
		"🧡", "🧡彩", `" OR "x"`, "a y b", "foo*", strings.Repeat("㌖", 1365),
		strings.Repeat("㌖", 682) + "X" + strings.Repeat("㌖", 682),
	}
	random := rand.New(rand.NewPCG(0x20261127, 0x9812cafd))
	alphabet := []string{"a", "a", "b", "c", "台", "北", "甲", "乙", "é", "e", "́", "/", "#", "*", " ", "🧡"}
	for range 150 {
		var text strings.Builder
		for range random.IntN(35) + 1 {
			text.WriteString(alphabet[random.IntN(len(alphabet))])
		}
		document := text.String()
		documents = append(documents, document)
		runes := []rune(document)
		start := random.IntN(len(runes))
		end := start + random.IntN(len(runes)-start+1)
		query := string(runes[start:end])
		if normalized, err := Normalize(query); err == nil && normalized != "" {
			queries = append(queries, query)
		}
	}
	encoded := make([]LiteralDocument, len(documents))
	for index, raw := range documents {
		var err error
		encoded[index], err = EncodeLiteralDocument(raw)
		if err != nil {
			t.Fatalf("document %d: %v", index, err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO candidate_fts(rowid,postings) VALUES(?,?)`, index+1, encoded[index].Postings); err != nil {
			t.Fatalf("insert document %d: %v", index, err)
		}
	}
	var sawFalseCandidate bool
	for queryIndex, raw := range queries {
		plan, err := CompileLiteralCandidates(raw)
		if err != nil {
			t.Fatalf("query %d: %v", queryIndex, err)
		}
		found := candidateRows(t, db, plan.MatchExpression())
		for index, doc := range encoded {
			want := strings.Contains(doc.Normalized, plan.pattern.needle)
			if want && !found[int64(index+1)] {
				t.Fatalf("query %d document %d: FTS omitted a literal match", queryIndex, index)
			}
			if !found[int64(index+1)] {
				continue
			}
			verified, err := plan.ContainsNormalized(doc.Normalized)
			if err != nil || verified != want {
				t.Fatalf("query %d document %d: verified=%t want=%t error=%v", queryIndex, index, verified, want, err)
			}
			if !want {
				sawFalseCandidate = true
			}
		}
	}
	if !sawFalseCandidate {
		t.Fatal("fixture did not exercise verification of false FTS candidates")
	}
}

func TestCandidateDistinctHanWithinInputBudget(t *testing.T) {
	db := candidateDB(t)
	characters := make([]rune, MaxInputBytes/3)
	for index := range characters {
		characters[index] = rune(0x4e00 + index)
	}
	doc, err := EncodeLiteralDocument(string(characters))
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(doc.Normalized)) != len(characters) || len(strings.Fields(doc.Postings)) != 2*len(characters)-1 {
		t.Fatal("distinct normalized Han postings were omitted or duplicated")
	}
	if len(doc.Postings) > MaxLiteralPostingsBytes {
		t.Fatal("postings exceed their encoded byte budget")
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO candidate_fts(rowid,postings) VALUES(1,?)`, doc.Postings); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		string(characters[0]),
		string(characters[len(characters)-1]),
		string(characters[len(characters)/2 : len(characters)/2+2]),
	} {
		plan, err := CompileLiteralCandidates(query)
		if err != nil {
			t.Fatal(err)
		}
		if !candidateRows(t, db, plan.MatchExpression())[1] {
			t.Fatal("FTS omitted a one- or two-character Han match")
		}
		matched, err := plan.ContainsNormalized(doc.Normalized)
		if err != nil || !matched {
			t.Fatalf("literal match missing: matched=%t err=%v", matched, err)
		}
	}
}

func TestCandidateLongTokensNeverReachSQLiteTruncation(t *testing.T) {
	db := candidateDB(t)
	doc, err := EncodeLiteralDocument(strings.Repeat("㌖", 1365))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Normalized) <= 4095 || len(doc.Postings) > MaxLiteralPostingsBytes {
		t.Fatal("fixture did not exercise bounded normalization expansion")
	}
	validPosting := regexp.MustCompile(`^[ub][0-9a-f]+$`)
	for _, term := range strings.Fields(doc.Postings) {
		if len(term) > 17 || !validPosting.MatchString(term) {
			t.Fatalf("invalid posting %q", term)
		}
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO candidate_fts(rowid,postings) VALUES(1,?)`, doc.Postings); err != nil {
		t.Fatal(err)
	}
	query := strings.Repeat("㌖", 682) + "X" + strings.Repeat("㌖", 682)
	plan, err := CompileLiteralCandidates(query)
	if err != nil {
		t.Fatal(err)
	}
	if !candidateRows(t, db, plan.MatchExpression())[1] {
		t.Fatal("fixture must remain a coarse candidate")
	}
	matched, err := plan.ContainsNormalized(doc.Normalized)
	if err != nil || matched {
		t.Fatalf("full literal verifier accepted a false candidate: %t %v", matched, err)
	}
}

func FuzzLiteralCandidateSuperset(f *testing.F) {
	for _, seed := range [][2]string{
		{"台北/東京", "北/東"}, {"Straße", "STRASSE"}, {"a x y b", "a y b"},
		{"甲/乙", "甲乙"}, {"🧡彩", "🧡"}, {"", ""}, {string([]byte{0xff}), "x"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, rawDocument, rawQuery string) {
		doc, docErr := EncodeLiteralDocument(rawDocument)
		plan, queryErr := CompileLiteralCandidates(rawQuery)
		if docErr != nil || queryErr != nil {
			return
		}
		matched, err := plan.ContainsNormalized(doc.Normalized)
		if err != nil {
			t.Fatal(err)
		}
		if matched != strings.Contains(doc.Normalized, plan.pattern.needle) {
			t.Fatal("literal verification differs from the reference")
		}
		if !matched {
			return
		}
		postings := make(map[string]bool)
		for _, term := range strings.Fields(doc.Postings) {
			postings[term] = true
		}
		for _, anchor := range strings.Split(plan.MatchExpression(), " AND ") {
			term := strings.Trim(anchor, `"`)
			if !postings[term] {
				t.Fatal("literal match omitted a required candidate anchor")
			}
		}
	})
}
