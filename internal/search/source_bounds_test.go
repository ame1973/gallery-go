package search

import (
	"encoding/json"
	"testing"

	"gallerygo/contracts"

	"golang.org/x/text/unicode/norm"
)

// The generator check establishes that absent mapping keys are identities.
// NFKD(casefold(s)) byte length supplies a weight: stable composition never
// increases it, and folding a stable scalar never exceeds it. Checking those
// invariants extends the per-scalar expansion bound across scalar boundaries.
// The bound is specific to the checked Unicode15 tables, not arbitrary versions.
func TestSourceTitleNormalizationExpansionBound(t *testing.T) {
	if pinnedProfileErr != nil || norm.Version != UnicodeVersion {
		t.Fatal("the pinned Unicode profile is unavailable")
	}
	var raw struct {
		Fold map[string]string `json:"fold"`
	}
	if err := json.Unmarshal([]byte(contracts.SearchUnicodeTables), &raw); err != nil {
		t.Fatal(err)
	}
	// A scalar missing from both tables is unchanged. Hangul is algorithmic.
	interesting := make(map[rune]struct{}, len(pinnedProfile.decomposition)+len(raw.Fold)+11172)
	for character := range pinnedProfile.decomposition {
		interesting[character] = struct{}{}
	}
	for key := range raw.Fold {
		characters := []rune(key)
		if len(characters) != 1 {
			t.Fatal("invalid fold key")
		}
		interesting[characters[0]] = struct{}{}
	}
	for character := rune(0xac00); character < 0xac00+11172; character++ {
		interesting[character] = struct{}{}
	}
	weight := func(character rune) int {
		return len(norm.NFKD.String(fold.String(string(character))))
	}
	stable := func(character rune) bool {
		text := string(character)
		return norm.NFKC.String(text) == text
	}
	checkStable := func(character rune) {
		t.Helper()
		if !stable(character) || len(fold.String(string(character))) > weight(character) {
			t.Fatalf("unstable scalar or growing fold weight: %U", character)
		}
	}
	for character := range interesting {
		text := string(character)
		decomposed := norm.NFKD.String(text)
		total := 0
		for _, component := range decomposed {
			checkStable(component)
			total += weight(component)
		}
		if len(decomposed) > 11*len(text) || total > 11*len(text) {
			t.Fatalf("source expansion exceeds its bound for %U", character)
		}
		if stable(character) {
			checkStable(character)
		}
	}
	checkPair := func(first, second, result rune) {
		t.Helper()
		if !stable(first) || !stable(second) {
			return // NFKD cannot yield this pair during the first stage.
		}
		checkStable(result)
		if len(string(result)) > len(string(first))+len(string(second)) ||
			weight(result) > weight(first)+weight(second) {
			t.Fatalf("composition increases its expansion weight: %U %U", first, second)
		}
	}
	for pair, result := range pinnedProfile.composition {
		checkPair(pair[0], pair[1], result)
	}
	for lead := rune(0); lead < 19; lead++ {
		for vowel := rune(0); vowel < 21; vowel++ {
			syllable := rune(0xac00) + (lead*21+vowel)*28
			checkPair(0x1100+lead, 0x1161+vowel, syllable)
			for trailing := rune(1); trailing < 28; trailing++ {
				checkPair(syllable, 0x11a7+trailing, syllable+trailing)
			}
		}
	}
	if 4096*11 > MaxNormalizedBytes {
		t.Fatal("the normalized byte budget is too small for bounded source titles")
	}
}
