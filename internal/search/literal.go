package search

import (
	"strings"
	"unicode/utf8"
)

// LiteralPattern verifies a complete literal, not an FTS candidate or access grant.
// Its compiled prefix table is immutable and can be reused concurrently.
type LiteralPattern struct {
	needle string
	prefix []uint32
}

// CompileLiteral normalizes one nonempty query using the pinned search profile.
// Punctuation remains literal; this does not define the complete search query syntax.
func CompileLiteral(query string) (LiteralPattern, error) {
	needle, err := Normalize(query)
	if err != nil {
		return LiteralPattern{}, err
	}
	if needle == "" {
		return LiteralPattern{}, ErrTextInvalid
	}

	prefix := make([]uint32, len(needle))
	matched := 0
	for index := 1; index < len(needle); index++ {
		for matched > 0 && needle[index] != needle[matched] {
			matched = int(prefix[matched-1])
		}
		if needle[index] == needle[matched] {
			matched++
		}
		prefix[index] = uint32(matched)
	}
	return LiteralPattern{needle: needle, prefix: prefix}, nil
}

// ContainsNormalized checks an already normalized document in linear byte time.
// The caller must obtain the document through fresh SQL authorization and ensure
// that it was produced by the same profile; this function does neither. Invalid
// UTF-8, NUL and documents exceeding the profile budget are errors, not misses.
func (pattern LiteralPattern) ContainsNormalized(document string) (bool, error) {
	if pattern.needle == "" {
		return false, ErrTextInvalid
	}
	if len(document) > MaxNormalizedBytes {
		return false, ErrTextLimit
	}
	if !utf8.ValidString(document) || strings.IndexByte(document, 0) >= 0 {
		return false, ErrTextInvalid
	}
	if len(pattern.needle) > len(document) {
		return false, nil
	}

	matched := 0
	for index := 0; index < len(document); index++ {
		for matched > 0 && document[index] != pattern.needle[matched] {
			matched = int(pattern.prefix[matched-1])
		}
		if document[index] == pattern.needle[matched] {
			matched++
			if matched == len(pattern.needle) {
				return true, nil
			}
		}
	}
	return false, nil
}
