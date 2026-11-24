// Command searchconformance emits deterministic Go-oracle cases for browser tests.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"gallerygo/internal/search"
)

type tables struct {
	Composition   map[string]string `json:"composition"`
	Decomposition map[string]string `json:"nfkd"`
	Combining     map[string]uint8  `json:"ccc"`
	Fold          map[string]string `json:"fold"`
}

type example struct {
	Input  string         `json:"input"`
	Tokens *search.Tokens `json:"tokens,omitempty"`
	Error  string         `json:"error,omitempty"`
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: searchconformance <unicode-table.json> <unicode-reference.txt.gz> <long-reference.json.gz>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var table tables
	if err := json.Unmarshal(data, &table); err != nil {
		panic(err)
	}
	cases := make([]example, 0, 55000)
	documents := make([]example, 0, 541)
	project := func(text string, tokenize func(string) (search.Tokens, error)) example {
		tokens, err := tokenize(text)
		item := example{Input: text}
		switch {
		case err == nil:
			item.Tokens = &tokens
		case errors.Is(err, search.ErrTextInvalid):
			item.Error = "invalid"
		case errors.Is(err, search.ErrTextLimit):
			item.Error = "limit"
		default:
			panic(err)
		}
		return item
	}
	add := func(text string) { cases = append(cases, project(text, search.Tokenize)) }
	addDocument := func(text string) { documents = append(documents, project(text, search.TokenizeDocument)) }
	if err := appendUnicodeReference(os.Args[2], add); err != nil {
		panic(err)
	}
	if err := appendLongReference(os.Args[3], func(text string) {
		add(text)
		addDocument(text)
	}); err != nil {
		panic(err)
	}
	for _, count := range []int{1024, 1025, 1365} {
		var text strings.Builder
		for index := range count {
			text.WriteRune(rune(0x4e00 + index))
		}
		addDocument(text.String())
	}
	addDocument(strings.Repeat("ﷺ", 1365))
	pairs := make([]string, 0, len(table.Composition))
	for pair := range table.Composition {
		pairs = append(pairs, pair)
	}
	sort.Strings(pairs)
	for _, pair := range pairs {
		parts := []rune(pair)
		if len(parts) != 2 {
			panic("composition key is not a scalar pair")
		}
		add(pair)
		add(string(parts[0]) + "̅" + string(parts[1]))
		add(string(parts[0]) + "̕" + string(parts[1]))
	}
	// Exercise every possible algorithmic Hangul decomposition/composition.
	for lead := rune(0); lead < 19; lead++ {
		for vowel := rune(0); vowel < 21; vowel++ {
			add(string([]rune{0x1100 + lead, 0x1161 + vowel}))
			for trailing := rune(1); trailing < 28; trailing++ {
				add(string([]rune{0x1100 + lead, 0x1161 + vowel, 0x11a7 + trailing}))
			}
		}
	}
	marks := make([]rune, 0, len(table.Combining))
	for scalar := range table.Combining {
		r, size := utf8.DecodeRuneInString(scalar)
		if size != len(scalar) {
			panic("combining table key is not one scalar")
		}
		marks = append(marks, r)
	}
	sort.Slice(marks, func(i, j int) bool { return marks[i] < marks[j] })
	alphabet := []rune{'A', 'a', 'ß', 'İ', 'Σ', 'ς', '台', '北', '中', '文', '9', '/', '#', ' ', '\u0085', 0xfeff, '꟱', '😀', 'ᄀ', 'ᅡ', 'ᆨ'}
	for scalar := range table.Decomposition {
		r, size := utf8.DecodeRuneInString(scalar)
		if size != len(scalar) {
			panic("decomposition table key is not one scalar")
		}
		alphabet = append(alphabet, r)
	}
	for scalar := range table.Fold {
		r, size := utf8.DecodeRuneInString(scalar)
		if size != len(scalar) {
			panic("case-fold table key is not one scalar")
		}
		alphabet = append(alphabet, r)
	}
	sort.Slice(alphabet, func(i, j int) bool { return alphabet[i] < alphabet[j] })
	random := rand.New(rand.NewPCG(0x12345678, 0x9abcdef0))
	for iteration := 0; iteration < 30000; iteration++ {
		length := int(random.Uint64N(16))
		text := make([]rune, 0, length)
		for range length {
			switch random.Uint64N(4) {
			case 0, 1:
				text = append(text, marks[random.Uint64N(uint64(len(marks)))])
			case 2:
				text = append(text, alphabet[random.Uint64N(uint64(len(alphabet)))])
			default:
				r := rune(random.Uint64N(0x110000))
				if !utf8.ValidRune(r) {
					r = '�'
				}
				text = append(text, r)
			}
		}
		add(string(text))
	}
	add("prefix\x00suffix")
	result := struct {
		Profile   string    `json:"profile"`
		Cases     []example `json:"cases"`
		Documents []example `json:"documents"`
	}{search.ProfileVersion, cases, documents}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		panic(err)
	}
}
