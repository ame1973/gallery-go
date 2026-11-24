// Package search defines the shared, versioned text contract for online and local
// indexes. It does not authorize resources or implement an index/query endpoint.
package search

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	ProfileVersion     = "gallerygo-search-v2-u15"
	UnicodeVersion     = "15.0.0"
	MaxInputBytes      = 16 << 10
	MaxNormalizedBytes = 64 << 10
	MaxSegmentUnits    = 6 * MaxNormalizedBytes
	MaxTokens          = 2048
	MaxDocumentTokens  = 2 * MaxNormalizedBytes
)

var (
	ErrTextInvalid = errors.New("invalid Unicode search text")
	ErrTextLimit   = errors.New("search text exceeds its normalization budget")
	ErrTextVersion = errors.New("search Unicode tables do not match the pinned profile")
	fold           = cases.Fold() // Fold is stateless and explicitly concurrent-safe.
)

// Normalize uses full default case folding, not locale-sensitive lowercasing.
// The second NFKC composes expansions introduced by folding. Categories and
// whitespace come from the pinned profile, independently of the Go runtime tables.
func Normalize(text string) (string, error) {
	if pinnedProfileErr != nil || norm.Version != UnicodeVersion || cases.UnicodeVersion != UnicodeVersion {
		return "", ErrTextVersion
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", ErrTextInvalid
	}
	if len(text) > MaxInputBytes {
		return "", ErrTextLimit
	}
	first, err := normalizeStage(text)
	if err != nil {
		return "", err
	}
	result, err := normalizeStage(fold.String(first))
	if err != nil {
		return "", err
	}
	return strings.TrimFunc(result, func(character rune) bool {
		return inUnicodeRanges(pinnedProfile.Space, character)
	}), nil
}

func normalizeStage(text string) (string, error) {
	if len(text) > MaxNormalizedBytes {
		return "", ErrTextLimit
	}
	// Stable ordering avoids x/text's stream-safe CGJ insertion and bounds work
	// even when a valid source name contains a long run of combining marks.
	type mark struct {
		character rune
		class     uint8
	}
	var ordered []rune
	var marks []mark
	flush := func() {
		if len(marks) > 1 {
			sort.SliceStable(marks, func(i, j int) bool { return marks[i].class < marks[j].class })
		}
		for _, item := range marks {
			ordered = append(ordered, item.character)
		}
		marks = marks[:0]
	}
	count := 0
	for _, original := range text {
		decomposed := string(original)
		index := original - 0xac00
		if index >= 0 && index < 11172 {
			lead := rune(0x1100) + index/588
			vowel := rune(0x1161) + (index%588)/28
			trailing := index % 28
			if trailing == 0 {
				decomposed = string([]rune{lead, vowel})
			} else {
				decomposed = string([]rune{lead, vowel, rune(0x11a7) + trailing})
			}
		} else if mapping, ok := pinnedProfile.decomposition[original]; ok {
			decomposed = mapping
		}
		for _, character := range decomposed {
			count++
			if count > 6*len(text) || count > MaxSegmentUnits {
				return "", ErrTextVersion
			}
			class := pinnedProfile.combining[character]
			if class == 0 {
				flush()
				ordered = append(ordered, character)
			} else {
				marks = append(marks, mark{character: character, class: class})
			}
		}
	}
	flush()
	if len(ordered) == 0 {
		return "", nil
	}
	result := []rune{ordered[0]}
	starterPosition, starter := 0, ordered[0]
	var previousClass uint8
	for _, character := range ordered[1:] {
		class := pinnedProfile.combining[character]
		if combined, ok := composeUnicode(starter, character); ok && (previousClass == 0 || previousClass < class) {
			result[starterPosition], starter = combined, combined
		} else {
			if class == 0 {
				starterPosition, starter = len(result), character
			}
			result = append(result, character)
			previousClass = class
		}
	}
	normalized := string(result)
	if len(normalized) > MaxNormalizedBytes {
		return "", ErrTextLimit
	}
	return normalized, nil
}

func composeUnicode(first, second rune) (rune, bool) {
	lead, vowel := first-0x1100, second-0x1161
	if lead >= 0 && lead < 19 && vowel >= 0 && vowel < 21 {
		return 0xac00 + (lead*21+vowel)*28, true
	}
	syllable, trailing := first-0xac00, second-0x11a7
	if syllable >= 0 && syllable < 11172 && syllable%28 == 0 && trailing > 0 && trailing < 28 {
		return first + trailing, true
	}
	value, ok := pinnedProfile.composition[[2]rune{first, second}]
	return value, ok
}

type Tokens struct {
	Normalized string   `json:"normalized"`
	Words      []string `json:"words"`
	CJKSingle  []string `json:"cjk_single"`
	CJKBigram  []string `json:"cjk_bigram"`
}

// Tokenize keeps first-occurrence order and separate short-CJK exact terms. No
// user text is converted into FTS syntax. Punctuation stays in Normalized for the
// literal matching contract; word tokens alone never prove a special-symbol match.
func Tokenize(text string) (Tokens, error) {
	return tokenize(text, MaxTokens)
}

// TokenizeDocument accepts every normalized input within the profile's byte
// bounds. Each scalar can contribute at most two distinct Han tokens.
func TokenizeDocument(text string) (Tokens, error) {
	return tokenize(text, MaxDocumentTokens)
}

func tokenize(text string, limit int) (Tokens, error) {
	normalized, err := Normalize(text)
	if err != nil {
		return Tokens{}, err
	}
	result := Tokens{Normalized: normalized, Words: []string{}, CJKSingle: []string{}, CJKBigram: []string{}}
	seen := [3]map[string]bool{{}, {}, {}}
	count := 0
	add := func(field int, word string) {
		if seen[field][word] {
			return
		}
		seen[field][word] = true
		count++
		switch field {
		case 0:
			result.Words = append(result.Words, word)
		case 1:
			result.CJKSingle = append(result.CJKSingle, word)
		case 2:
			result.CJKBigram = append(result.CJKBigram, word)
		}
	}
	var word []rune
	flush := func() {
		if len(word) != 0 {
			add(0, string(word))
			word = word[:0]
		}
	}
	var previous rune
	for _, character := range normalized {
		if inUnicodeRanges(pinnedProfile.Han, character) {
			flush()
			add(1, string(character))
			if previous != 0 {
				add(2, string([]rune{previous, character}))
			}
			previous = character
		} else {
			previous = 0
			if inUnicodeRanges(pinnedProfile.Words, character) {
				word = append(word, character)
			} else {
				flush()
			}
		}
		if count > limit {
			return Tokens{}, ErrTextLimit
		}
	}
	flush()
	if count > limit {
		return Tokens{}, ErrTextLimit
	}
	return result, nil
}
