package search

import (
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
)

func TestLiteralMatchesNormalizedText(t *testing.T) {
	for _, sample := range []struct {
		query    string
		document string
		want     bool
	}{
		{" STRASSE ", "A Straße!", true},
		{"台", "臺北和台中", true},
		{"台北", "旅遊：台北！", true},
		{"北/東", "台北/東京", true},
		{"北東京", "台北/東京", false},
		{"#1", "THE—end #1", true},
		{"foo*", "a foo* b", true},
		{"foo*", "a foo b", false},
		{"ababaca", "zzabababacayy", true},
		{"ababaca", "zzabababababyy", false},
		{"a", "", false},
	} {
		document, err := Normalize(sample.document)
		if err != nil {
			t.Fatal(err)
		}
		pattern, err := CompileLiteral(sample.query)
		if err != nil {
			t.Fatalf("compile %q: %v", sample.query, err)
		}
		got, err := pattern.ContainsNormalized(document)
		if err != nil || got != sample.want {
			t.Fatalf("query=%q document=%q got=%t want=%t err=%v", sample.query, sample.document, got, sample.want, err)
		}
	}
}

func TestLiteralRejectsInvalidOrUnboundedInputs(t *testing.T) {
	for _, input := range []string{"", "  ", "\x00", string([]byte{0xff})} {
		if _, err := CompileLiteral(input); !errors.Is(err, ErrTextInvalid) {
			t.Fatalf("query %q: got %v, want invalid", input, err)
		}
	}
	if _, err := CompileLiteral(strings.Repeat("a", MaxInputBytes+1)); !errors.Is(err, ErrTextLimit) {
		t.Fatalf("oversized query: %v", err)
	}
	pattern, err := CompileLiteral("a")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"\x00", string([]byte{0xff})} {
		if matched, err := pattern.ContainsNormalized(input); matched || !errors.Is(err, ErrTextInvalid) {
			t.Fatalf("document %q: matched=%t err=%v", input, matched, err)
		}
	}
	if matched, err := pattern.ContainsNormalized(strings.Repeat("a", MaxNormalizedBytes+1)); matched || !errors.Is(err, ErrTextLimit) {
		t.Fatalf("oversized document: matched=%t err=%v", matched, err)
	}
	if matched, err := (LiteralPattern{}).ContainsNormalized("a"); matched || !errors.Is(err, ErrTextInvalid) {
		t.Fatalf("zero pattern: matched=%t err=%v", matched, err)
	}
	if matched, err := pattern.ContainsNormalized(strings.Repeat("a", MaxNormalizedBytes)); !matched || err != nil {
		t.Fatalf("boundary document: matched=%t err=%v", matched, err)
	}
	boundary, err := CompileLiteral(strings.Repeat("a", MaxInputBytes))
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := boundary.ContainsNormalized(strings.Repeat("a", MaxInputBytes)); !matched || err != nil {
		t.Fatalf("boundary query: matched=%t err=%v", matched, err)
	}
}

func TestLiteralDifferential(t *testing.T) {
	random := rand.New(rand.NewPCG(0x20261127, 0x9af8c113))
	alphabet := []string{"a", "a", "b", "c", "台", "北", "é", "e", "́", "/", "#", " "}
	makeText := func(count int) string {
		var text strings.Builder
		for range count {
			text.WriteString(alphabet[random.IntN(len(alphabet))])
		}
		return text.String()
	}
	for iteration := range 5000 {
		rawDocument := makeText(random.IntN(100))
		query := makeText(random.IntN(30))
		if iteration%3 == 0 {
			runes := []rune(rawDocument)
			start := random.IntN(len(runes) + 1)
			end := start + random.IntN(len(runes)-start+1)
			query = string(runes[start:end])
		}
		document, err := Normalize(rawDocument)
		if err != nil {
			t.Fatal(err)
		}
		needle, err := Normalize(query)
		if err != nil {
			t.Fatal(err)
		}
		pattern, err := CompileLiteral(query)
		if needle == "" {
			if !errors.Is(err, ErrTextInvalid) {
				t.Fatalf("iteration %d: empty query error %v", iteration, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		got, err := pattern.ContainsNormalized(document)
		if want := strings.Contains(document, needle); err != nil || got != want {
			t.Fatalf("iteration %d: got=%t want=%t err=%v", iteration, got, want, err)
		}
	}
}

func TestLiteralExpandedProfileBoundary(t *testing.T) {
	query := strings.Repeat("㌖", 1365)
	needle, err := Normalize(query)
	if err != nil {
		t.Fatal(err)
	}
	if len(needle) <= len(query) {
		t.Fatal("expected compatibility expansion")
	}
	pattern, err := CompileLiteral(query)
	if err != nil {
		t.Fatal(err)
	}
	if len(pattern.prefix) != len(needle) || len(pattern.prefix) > MaxNormalizedBytes {
		t.Fatal("invalid prefix-table bound")
	}
	if matched, err := pattern.ContainsNormalized(needle); !matched || err != nil {
		t.Fatalf("expanded query: matched=%t err=%v", matched, err)
	}
	if matched, err := pattern.ContainsNormalized(needle[:len(needle)-3] + "abc"); matched || err != nil {
		t.Fatalf("different expanded document: matched=%t err=%v", matched, err)
	}
}

func TestLiteralAdversarialAndConcurrent(t *testing.T) {
	document := strings.Repeat("台", 1363) + "叝叙"
	query := strings.Repeat("台", 677) + "参叄参又叝叙"
	pattern, err := CompileLiteral(query)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			for range 50 {
				got, err := pattern.ContainsNormalized(document)
				if got || err != nil {
					t.Errorf("nonmatching collision document: got=%t err=%v", got, err)
					return
				}
			}
		})
	}
	group.Wait()
}

func BenchmarkLiteralCollision(b *testing.B) {
	document := strings.Repeat("台", 1363) + "叝叙"
	query := strings.Repeat("台", 677) + "参叄参又叝叙"
	pattern, err := CompileLiteral(query)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("compiled", func(b *testing.B) {
		var matched bool
		for b.Loop() {
			matched, err = pattern.ContainsNormalized(document)
			if err != nil {
				b.Fatal(err)
			}
		}
		if matched {
			b.Fatal("unexpected match")
		}
	})
	b.Run("strings-contains", func(b *testing.B) {
		var matched bool
		for b.Loop() {
			matched = strings.Contains(document, pattern.needle)
		}
		if matched {
			b.Fatal("unexpected match")
		}
	})
}

func FuzzLiteralAgainstContains(f *testing.F) {
	for _, seed := range [][2]string{
		{"台北/東京", "北/東"},
		{"Straße", "STRASSE"},
		{"abababababac", "ababac"},
		{"!#*", "#"},
		{"", ""},
		{string([]byte{0xff}), "x"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, rawDocument, query string) {
		document, docErr := Normalize(rawDocument)
		needle, queryErr := Normalize(query)
		pattern, err := CompileLiteral(query)
		if queryErr != nil {
			if !errors.Is(err, queryErr) {
				t.Fatalf("query errors differ: %v / %v", err, queryErr)
			}
			return
		}
		if needle == "" {
			if !errors.Is(err, ErrTextInvalid) {
				t.Fatalf("empty query: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if docErr != nil {
			return
		}
		got, err := pattern.ContainsNormalized(document)
		if want := strings.Contains(document, needle); err != nil || got != want {
			t.Fatalf("got=%t want=%t err=%v", got, want, err)
		}
	})
}
