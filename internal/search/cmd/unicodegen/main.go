// Command unicodegen emits the pinned browser tables and scalar conformance hash.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"unicode/utf8"

	"gallerygo/contracts"
	"gallerygo/internal/search"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type span [2]uint32

type tables struct {
	Profile           string            `json:"profile"`
	Unicode           string            `json:"unicode"`
	MaxInput          int               `json:"max_input_bytes"`
	MaxOutput         int               `json:"max_normalized_bytes"`
	MaxSegment        int               `json:"max_segment_units"`
	MaxTokens         int               `json:"max_tokens"`
	MaxDocumentTokens int               `json:"max_document_tokens"`
	Known             []span            `json:"known"`
	Words             []span            `json:"words"`
	Han               []span            `json:"han"`
	Space             []span            `json:"space"`
	NoBoundary        []span            `json:"no_boundary"`
	Fold              map[string]string `json:"fold"`
	Decomposition     map[string]string `json:"nfkd"`
	Combining         map[string]uint8  `json:"ccc"`
	Composition       map[string]string `json:"composition"`
	ScalarSHA256      string            `json:"scalar_sha256"`
}

func ranges(predicate func(rune) bool) []span {
	result := []span{}
	for character := rune(0); character <= utf8.MaxRune; character++ {
		if !utf8.ValidRune(character) || !predicate(character) {
			continue
		}
		last := len(result) - 1
		if last >= 0 && result[last][1]+1 == uint32(character) {
			result[last][1]++
		} else {
			result = append(result, span{uint32(character), uint32(character)})
		}
	}
	return result
}

func generate() ([]byte, error) {
	if norm.Version != search.UnicodeVersion || cases.UnicodeVersion != search.UnicodeVersion {
		return nil, search.ErrTextVersion
	}
	// Categories are frozen inputs, not the current Go runtime's Unicode tables.
	var seed tables
	if err := json.Unmarshal([]byte(contracts.SearchUnicodeTables), &seed); err != nil {
		return nil, err
	}
	if seed.Profile != search.ProfileVersion || seed.Unicode != search.UnicodeVersion || len(seed.Known) == 0 {
		return nil, search.ErrTextVersion
	}
	known := func(r rune) bool {
		index := sort.Search(len(seed.Known), func(index int) bool { return seed.Known[index][1] >= uint32(r) })
		return index < len(seed.Known) && seed.Known[index][0] <= uint32(r)
	}
	fold := cases.Fold()
	data := tables{
		Profile: search.ProfileVersion, Unicode: search.UnicodeVersion,
		MaxInput: search.MaxInputBytes, MaxOutput: search.MaxNormalizedBytes,
		MaxSegment: search.MaxSegmentUnits, MaxTokens: search.MaxTokens,
		MaxDocumentTokens: search.MaxDocumentTokens,
		Known:             seed.Known,
		Words:             seed.Words,
		Han:               seed.Han,
		Space:             seed.Space,
		NoBoundary:        ranges(func(r rune) bool { return known(r) && !norm.NFKD.PropertiesString(string(r)).BoundaryBefore() }),
		Fold:              make(map[string]string),
		Decomposition:     make(map[string]string),
		Combining:         make(map[string]uint8),
		Composition:       make(map[string]string),
	}
	digest := sha256.New()
	var header [9]byte
	for character := rune(0); character <= utf8.MaxRune; character++ {
		if !utf8.ValidRune(character) {
			continue
		}
		text := string(character)
		if folded := fold.String(text); folded != text {
			data.Fold[text] = folded
		}
		if ccc := norm.NFKD.PropertiesString(text).CCC(); ccc != 0 {
			data.Combining[text] = ccc
		}
		// Hangul syllables use the standard arithmetic decomposition/composition
		// instead of storing more than eleven thousand mechanical entries.
		if character < 0xac00 || character >= 0xac00+11172 {
			if decomposed := norm.NFKD.String(text); decomposed != text {
				data.Decomposition[text] = decomposed
			}
			canonical := norm.NFD.String(text)
			if canonical != text && norm.NFC.String(canonical) == text {
				parts := []rune(canonical)
				if len(parts) < 2 {
					return nil, fmt.Errorf("invalid canonical decomposition for U+%04X", character)
				}
				prefix := []rune(norm.NFC.String(string(parts[:len(parts)-1])))
				if len(prefix) != 1 {
					return nil, fmt.Errorf("canonical prefix does not compose for U+%04X", character)
				}
				key := string([]rune{prefix[0], parts[len(parts)-1]})
				if previous, found := data.Composition[key]; found && previous != text {
					return nil, fmt.Errorf("conflicting canonical composition for U+%04X", character)
				}
				data.Composition[key] = text
			}
		}
		normalized, err := search.Normalize(text)
		var code byte
		switch {
		case err == nil:
		case errors.Is(err, search.ErrTextInvalid):
			code = 1
		case errors.Is(err, search.ErrTextLimit):
			code = 2
		default:
			return nil, err
		}
		binary.LittleEndian.PutUint32(header[:4], uint32(character))
		header[4] = code
		binary.LittleEndian.PutUint32(header[5:], uint32(len(normalized)))
		digest.Write(header[:])
		digest.Write([]byte(normalized))
	}
	data.ScalarSHA256 = hex.EncodeToString(digest.Sum(nil))
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func main() {
	check := flag.Bool("check", false, "Check deterministic table content without writing")
	output := flag.String("output", "contracts/search-normalization-tables.json", "Generated product table file")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected generator arguments")
		os.Exit(1)
	}
	data, err := generate()
	if err == nil && *check {
		var existing []byte
		existing, err = os.ReadFile(*output)
		if err == nil {
			// Formatting tools may wrap JSON arrays differently; values/order remain
			// deterministic. Compare compact JSON, not cosmetic indentation.
			var left, right bytes.Buffer
			if err = json.Compact(&left, existing); err == nil {
				err = json.Compact(&right, data)
			}
			if err == nil && !bytes.Equal(left.Bytes(), right.Bytes()) {
				err = errors.New("search Unicode tables are stale; regenerate them")
			}
		}
	} else if err == nil {
		err = os.WriteFile(*output, data, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
