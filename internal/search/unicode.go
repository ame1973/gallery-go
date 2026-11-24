package search

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"gallerygo/contracts"
)

type unicodeRange [2]uint32

type unicodeProfile struct {
	Profile           string            `json:"profile"`
	Unicode           string            `json:"unicode"`
	MaxInput          int               `json:"max_input_bytes"`
	MaxOutput         int               `json:"max_normalized_bytes"`
	MaxSegment        int               `json:"max_segment_units"`
	MaxTokens         int               `json:"max_tokens"`
	MaxDocumentTokens int               `json:"max_document_tokens"`
	Known             []unicodeRange    `json:"known"`
	Words             []unicodeRange    `json:"words"`
	Han               []unicodeRange    `json:"han"`
	Space             []unicodeRange    `json:"space"`
	ScalarHash        string            `json:"scalar_sha256"`
	RawDecomposition  map[string]string `json:"nfkd"`
	RawCombining      map[string]uint8  `json:"ccc"`
	RawComposition    map[string]string `json:"composition"`
	RawFold           map[string]string `json:"fold"`
	decomposition     map[rune]string
	combining         map[rune]uint8
	composition       map[[2]rune]rune
}

var pinnedProfile, pinnedProfileErr = loadUnicodeProfile(contracts.SearchUnicodeTables)

func loadUnicodeProfile(data string) (unicodeProfile, error) {
	var profile unicodeProfile
	if err := json.Unmarshal([]byte(data), &profile); err != nil {
		return unicodeProfile{}, err
	}
	if profile.Profile != ProfileVersion || profile.Unicode != UnicodeVersion ||
		profile.MaxInput != MaxInputBytes || profile.MaxOutput != MaxNormalizedBytes ||
		profile.MaxSegment != MaxSegmentUnits || profile.MaxTokens != MaxTokens ||
		profile.MaxDocumentTokens != MaxDocumentTokens ||
		profile.ScalarHash != "55aa5a8ac475f0689b58bca3624881e337d639771941a01c17f0e19c67830d13" {
		return unicodeProfile{}, errors.New("incompatible Unicode search profile")
	}
	for _, spans := range [][]unicodeRange{profile.Known, profile.Words, profile.Han, profile.Space} {
		if len(spans) == 0 {
			return unicodeProfile{}, errors.New("empty Unicode search category")
		}
		for index, span := range spans {
			if span[0] > span[1] || span[1] > utf8.MaxRune ||
				(span[0] <= 0xdfff && span[1] >= 0xd800) ||
				(index > 0 && spans[index-1][1] >= span[0]) {
				return unicodeProfile{}, errors.New("invalid Unicode search category")
			}
		}
	}
	if len(profile.RawDecomposition) == 0 || len(profile.RawCombining) == 0 ||
		len(profile.RawComposition) == 0 || len(profile.RawFold) == 0 {
		return unicodeProfile{}, errors.New("empty Unicode normalization table")
	}
	profile.decomposition = make(map[rune]string, len(profile.RawDecomposition))
	for key, value := range profile.RawDecomposition {
		character, size := utf8.DecodeRuneInString(key)
		if !utf8.ValidString(key) || size == 0 || size != len(key) || character == 0 || !utf8.ValidString(value) ||
			value == "" || strings.ContainsRune(value, 0) || len(value) > 11*len(key) ||
			utf8.RuneCountInString(value) > 6*len(key) {
			return unicodeProfile{}, errors.New("invalid Unicode decomposition table")
		}
		profile.decomposition[character] = value
	}
	profile.combining = make(map[rune]uint8, len(profile.RawCombining))
	for key, value := range profile.RawCombining {
		character, size := utf8.DecodeRuneInString(key)
		if !utf8.ValidString(key) || size == 0 || size != len(key) || character == 0 || value == 0 {
			return unicodeProfile{}, errors.New("invalid Unicode combining table")
		}
		profile.combining[character] = value
	}
	profile.composition = make(map[[2]rune]rune, len(profile.RawComposition))
	for key, value := range profile.RawComposition {
		pair, output := []rune(key), []rune(value)
		if !utf8.ValidString(key) || !utf8.ValidString(value) || len(pair) != 2 || len(output) != 1 ||
			pair[0] == 0 || pair[1] == 0 || output[0] == 0 {
			return unicodeProfile{}, errors.New("invalid Unicode composition table")
		}
		profile.composition[[2]rune{pair[0], pair[1]}] = output[0]
	}
	for key, value := range profile.RawFold {
		character, size := utf8.DecodeRuneInString(key)
		if !utf8.ValidString(key) || size == 0 || size != len(key) || character == 0 || !utf8.ValidString(value) ||
			value == "" || strings.ContainsRune(value, 0) || len(value) > 3*len(key) {
			return unicodeProfile{}, errors.New("invalid Unicode fold table")
		}
	}
	profile.RawDecomposition, profile.RawCombining, profile.RawComposition, profile.RawFold = nil, nil, nil, nil
	return profile, nil
}

func inUnicodeRanges(spans []unicodeRange, character rune) bool {
	value := uint32(character)
	index := sort.Search(len(spans), func(index int) bool { return spans[index][1] >= value })
	return index < len(spans) && spans[index][0] <= value
}
