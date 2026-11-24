package search

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func TestNormalizePinnedProfile(t *testing.T) {
	if norm.Version != UnicodeVersion || cases.UnicodeVersion != UnicodeVersion {
		t.Fatal("x/text does not match the pinned Unicode profile", norm.Version, cases.UnicodeVersion)
	}
	for _, test := range []struct{ input, want string }{
		{" ＦＯＯ　Straße ", "foo strasse"},
		{"Σς", "σσ"},
		{"İ", "i̇"},
		{"Kﬁ", "kfi"},
		{"Å", "å"},
		{"臺北 / 台北 #1", "臺北 / 台北 #1"},
		{"Ᲊ", "Ᲊ"},
		{"꟱", "꟱"}, // Unassigned in Unicode 15; newer native NFKC differs.
		{"\u0085A\u0085", "a"},
		{string([]rune{0xfeff, 'A', 0xfeff}), string([]rune{0xfeff, 'a', 0xfeff})},
	} {
		got, err := Normalize(test.input)
		if err != nil || got != test.want {
			t.Fatalf("Normalize(%q)=%q, want %q: %v", test.input, got, test.want, err)
		}
	}
}

func TestNormalizeRejectsInvalidAndUnboundedText(t *testing.T) {
	for _, text := range []string{string([]byte{0xff}), "prefix\x00suffix"} {
		if _, err := Normalize(text); !errors.Is(err, ErrTextInvalid) {
			t.Fatal("invalid text was accepted", err)
		}
	}
	if _, err := Normalize(strings.Repeat("x", MaxInputBytes+1)); !errors.Is(err, ErrTextLimit) {
		t.Fatal("oversized input was accepted", err)
	}
	if _, err := Normalize("a" + strings.Repeat("́", (MaxInputBytes-1)/2)); err != nil {
		t.Fatal("boundary-sized combining sequence was rejected", err)
	}
	if _, err := Normalize("a" + strings.Repeat("́", (MaxInputBytes-1)/2+1)); !errors.Is(err, ErrTextLimit) {
		t.Fatal("oversized input was accepted", err)
	}
}

func TestTokenizeSeparatesHanBigramsAndKeepsLiteralPunctuation(t *testing.T) {
	got, err := Tokenize("  台北 ABC-12Ａ / 台北  ")
	want := Tokens{
		Normalized: "台北 abc-12a / 台北",
		Words:      []string{"abc", "12a"},
		CJKSingle:  []string{"台", "北"},
		CJKBigram:  []string{"台北"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("unexpected normalized token projection", got, want, err)
	}
	got, err = Tokenize("甲/乙")
	if err != nil || len(got.CJKBigram) != 0 || got.Normalized != "甲/乙" {
		t.Fatal("tokenization bridged literal punctuation", got, err)
	}
}

func TestNormalizeConcurrentFold(t *testing.T) {
	const input, want = "  ＦＯＯ Straße 台北  ", "foo strasse 台北"
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			for range 50 {
				got, err := Normalize(input)
				if err != nil || got != want {
					t.Errorf("concurrent normalization=%q: %v", got, err)
					return
				}
			}
		})
	}
	group.Wait()
}
