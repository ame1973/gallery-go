package search

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestPinnedUnicodeCategoriesAgainstUnicode15(t *testing.T) {
	if pinnedProfileErr != nil {
		t.Fatal(pinnedProfileErr)
	}
	if unicode.Version != UnicodeVersion {
		t.Skip("the runtime uses different Unicode tables; shared fixtures still test the pinned profile")
	}
	for character := rune(0); character <= utf8.MaxRune; character++ {
		if !utf8.ValidRune(character) {
			continue
		}
		if got, want := inUnicodeRanges(pinnedProfile.Known, character), !unicode.Is(unicode.Cn, character); got != want {
			t.Fatalf("known category differs for U+%04X: got %t want %t", character, got, want)
		}
		if got, want := inUnicodeRanges(pinnedProfile.Words, character), unicode.IsLetter(character) || unicode.IsNumber(character) || unicode.IsMark(character); got != want {
			t.Fatalf("word category differs for U+%04X: got %t want %t", character, got, want)
		}
		if got, want := inUnicodeRanges(pinnedProfile.Han, character), unicode.Is(unicode.Han, character); got != want {
			t.Fatalf("Han category differs for U+%04X: got %t want %t", character, got, want)
		}
		if got, want := inUnicodeRanges(pinnedProfile.Space, character), unicode.IsSpace(character); got != want {
			t.Fatalf("space category differs for U+%04X: got %t want %t", character, got, want)
		}
	}
}

func TestLoadUnicodeProfileRejectsCorruptMetadataAndRanges(t *testing.T) {
	base := fmt.Sprintf(`{"profile":%q,"unicode":%q,"max_input_bytes":%d,"max_normalized_bytes":%d,"max_segment_units":%d,"max_tokens":%d,"max_document_tokens":%d,"known":[[0,127]],"words":[[65,90]],"han":[[19968,19969]],"space":[[9,13]],"nfkd":{"ﬁ":"fi"},"ccc":{"̀":230},"composition":{"à":"à"},"fold":{"A":"a"},"scalar_sha256":%q}`,
		ProfileVersion, UnicodeVersion, MaxInputBytes, MaxNormalizedBytes, MaxSegmentUnits, MaxTokens, MaxDocumentTokens,
		"55aa5a8ac475f0689b58bca3624881e337d639771941a01c17f0e19c67830d13")
	if _, err := loadUnicodeProfile(base); err != nil {
		t.Fatal("valid test profile was rejected", err)
	}
	for name, input := range map[string]string{
		"malformed":               "{",
		"version":                 strings.Replace(base, ProfileVersion, "another-profile", 1),
		"limits":                  strings.Replace(base, `"max_tokens":2048`, `"max_tokens":1`, 1),
		"checksum":                strings.Replace(base, `55aa5a8a`, `00000000`, 1),
		"empty":                   strings.Replace(base, `"words":[[65,90]]`, `"words":[]`, 1),
		"descending":              strings.Replace(base, `"words":[[65,90]]`, `"words":[[90,65]]`, 1),
		"overlap":                 strings.Replace(base, `"words":[[65,90]]`, `"words":[[65,90],[90,100]]`, 1),
		"surrogate":               strings.Replace(base, `"words":[[65,90]]`, `"words":[[55295,57344]]`, 1),
		"above-unicode":           strings.Replace(base, `"words":[[65,90]]`, `"words":[[65,1114112]]`, 1),
		"negative-scalar":         strings.Replace(base, `"words":[[65,90]]`, `"words":[[-1,90]]`, 1),
		"document-limits":         strings.Replace(base, `"max_document_tokens":131072`, `"max_document_tokens":1`, 1),
		"empty-decomposition":     strings.Replace(base, `"nfkd":{"ﬁ":"fi"}`, `"nfkd":{}`, 1),
		"invalid-decomposition":   strings.Replace(base, `"nfkd":{"ﬁ":"fi"}`, `"nfkd":{"":"fi"}`, 1),
		"oversized-decomposition": strings.Replace(base, `"nfkd":{"ﬁ":"fi"}`, fmt.Sprintf(`"nfkd":{"ﬁ":%q}`, strings.Repeat("x", 34)), 1),
		"invalid-combining":       strings.Replace(base, `"ccc":{"̀":230}`, `"ccc":{"̀":0}`, 1),
		"invalid-composition":     strings.Replace(base, `"composition":{"à":"à"}`, `"composition":{"a":"à"}`, 1),
		"invalid-fold":            strings.Replace(base, `"fold":{"A":"a"}`, `"fold":{"A":"abcd"}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadUnicodeProfile(input); err == nil {
				t.Fatal("corrupt Unicode profile was accepted")
			}
		})
	}
	if inUnicodeRanges(pinnedProfile.Words, 0xa7f1) {
		t.Fatal("a character unassigned in Unicode 15 entered the pinned word category")
	}
}
