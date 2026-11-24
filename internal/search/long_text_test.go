package search

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeValidLongCombiningSequence(t *testing.T) {
	input := "a" + strings.Repeat("̀", 24)
	want := "à" + strings.Repeat("̀", 23)
	got, err := Normalize(input)
	if err != nil || got != want {
		t.Fatalf("valid long-combining basename normalized incorrectly: got %q want %q: %v", got, want, err)
	}
}

func TestLongCombiningSequenceConcurrent(t *testing.T) {
	const repetitions = 2047
	input := "A" + strings.Repeat("̴̧̖̕", repetitions)
	want := "a" + strings.Repeat("̴", repetitions) + strings.Repeat("̧", repetitions) +
		strings.Repeat("̖", repetitions) + strings.Repeat("̕", repetitions)
	if len(input) != 16377 {
		t.Fatal("unexpected input size")
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			for range 5 {
				got, err := Normalize(input)
				if err != nil || got != want {
					t.Errorf("concurrent long normalization mismatch: %v", err)
					return
				}
			}
		})
	}
	group.Wait()
}

func TestDocumentNormalizesLargestScalarExpansionWithinTitleLimit(t *testing.T) {
	input := strings.Repeat("ﷺ", 1365)
	want := strings.Repeat("صلى الله عليه وسلم", 1365)
	if len(input) != 4095 || len(want) != 45045 {
		t.Fatal("unexpected expansion fixture size")
	}
	tokens, err := TokenizeDocument(input)
	if err != nil || tokens.Normalized != want {
		t.Fatalf("expanded source title was not tokenized correctly: %v", err)
	}
}

func BenchmarkNormalizeLongCombiningSequence(b *testing.B) {
	input := "A" + strings.Repeat("̴̧̖̕", 2047)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Normalize(input); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDocumentTokenBudgetCoversBoundedSourceTitles(t *testing.T) {
	for _, count := range []int{1024, 1025, 1365} {
		var text strings.Builder
		for index := range count {
			text.WriteRune(rune(0x4e00 + index))
		}
		input := text.String()
		if len(input) > 4096 {
			t.Fatal("fixture exceeds the public source-title bound")
		}
		tokens, err := TokenizeDocument(input)
		if err != nil || len(tokens.CJKSingle) != count || len(tokens.CJKBigram) != count-1 {
			t.Fatalf("document tokens: singles=%d bigrams=%d error=%v", len(tokens.CJKSingle), len(tokens.CJKBigram), err)
		}
		_, err = Tokenize(input)
		if count == 1024 && err != nil {
			t.Fatalf("within-budget query rejected: %v", err)
		}
		if count > 1024 && !errors.Is(err, ErrTextLimit) {
			t.Fatalf("oversized query token set was not rejected: %v", err)
		}
	}
	if _, err := TokenizeDocument("bad\x00name"); !errors.Is(err, ErrTextInvalid) {
		t.Fatalf("invalid document was not rejected: %v", err)
	}
}
