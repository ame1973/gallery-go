package search

import (
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

const (
	// LiteralPostingsVersion must change when the encoding or pinned profile changes.
	LiteralPostingsVersion = ProfileVersion + "-literal-bigram-v1"
	// Each normalized byte can contribute at most one scalar and one bigram;
	// each ASCII posting plus separator occupies at most 18 bytes.
	MaxLiteralPostingsBytes = 36 * MaxNormalizedBytes
)

// LiteralDocument is an indexable projection, not an authorization grant.
type LiteralDocument struct {
	Normalized string
	Postings   string
}

// EncodeLiteralDocument builds deduplicated scalar and adjacent-scalar postings.
// ASCII hex keeps all generated tokens below SQLite's token length limit and
// prevents user text from becoming FTS syntax. The projection is not self-updating.
func EncodeLiteralDocument(text string) (LiteralDocument, error) {
	normalized, err := Normalize(text)
	if err != nil {
		return LiteralDocument{}, err
	}
	var postings strings.Builder
	seen := make(map[string]struct{})
	add := func(term string) error {
		if _, exists := seen[term]; exists {
			return nil
		}
		additional := len(term)
		if postings.Len() != 0 {
			additional++
		}
		if additional > MaxLiteralPostingsBytes-postings.Len() {
			return ErrTextLimit
		}
		seen[term] = struct{}{}
		if postings.Len() != 0 {
			postings.WriteByte(' ')
		}
		postings.WriteString(term)
		return nil
	}
	previous := ""
	for _, character := range normalized {
		current := string(character)
		if err := add(literalTerm('u', current)); err != nil {
			return LiteralDocument{}, err
		}
		if previous != "" {
			if err := add(literalTerm('b', previous+current)); err != nil {
				return LiteralDocument{}, err
			}
		}
		previous = current
	}
	return LiteralDocument{Normalized: normalized, Postings: postings.String()}, nil
}

func literalTerm(prefix byte, text string) string {
	return string(prefix) + hex.EncodeToString([]byte(text))
}

// LiteralCandidates uses at most two necessary anchors; false positives must be
// checked with ContainsNormalized after the complete SQL visibility predicate.
type LiteralCandidates struct {
	expression string
	pattern    LiteralPattern
}

// CompileLiteralCandidates generates only quoted, ASCII-safe FTS terms. Supply
// MatchExpression as a bound MATCH parameter, never as SQL text. This is not the
// complete search syntax and cannot establish index freshness or authorization.
func CompileLiteralCandidates(query string) (LiteralCandidates, error) {
	pattern, err := CompileLiteral(query)
	if err != nil {
		return LiteralCandidates{}, err
	}
	text := pattern.needle
	_, firstSize := utf8.DecodeRuneInString(text)
	first := literalTerm('u', text[:firstSize])
	last := first
	if firstSize < len(text) {
		_, secondSize := utf8.DecodeRuneInString(text[firstSize:])
		first = literalTerm('b', text[:firstSize+secondSize])
		_, lastSize := utf8.DecodeLastRuneInString(text)
		lastStart := len(text) - lastSize
		_, previousSize := utf8.DecodeLastRuneInString(text[:lastStart])
		last = literalTerm('b', text[lastStart-previousSize:])
	}
	expression := `"` + first + `"`
	if last != first {
		expression += ` AND "` + last + `"`
	}
	return LiteralCandidates{expression: expression, pattern: pattern}, nil
}

// MatchExpression is an FTS5 MATCH parameter, not an SQL query or access decision.
func (candidates LiteralCandidates) MatchExpression() string {
	return candidates.expression
}

// ContainsNormalized checks the entire literal; a matching anchor alone is not a hit.
func (candidates LiteralCandidates) ContainsNormalized(document string) (bool, error) {
	return candidates.pattern.ContainsNormalized(document)
}
