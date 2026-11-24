package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
)

// These examples are intended to be shared with the browser search implementation.
func TestSharedSearchFixtures(t *testing.T) {
	data, err := os.ReadFile("../../contracts/search-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Profile string `json:"profile"`
		Cases   []struct {
			Name  string `json:"name"`
			Input string `json:"input"`
			Tokens
		} `json:"cases"`
		Invalid []struct {
			Name  string `json:"name"`
			Input string `json:"input"`
		} `json:"invalid"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatal("fixture has trailing JSON", err)
	}
	if fixtures.Profile != ProfileVersion || len(fixtures.Cases) == 0 || len(fixtures.Invalid) == 0 {
		t.Fatal("fixture profile or cases are missing")
	}
	seen := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || seen[fixture.Name] {
			t.Fatal("fixture names must be unique and nonempty", fixture.Name)
		}
		seen[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			got, err := Tokenize(fixture.Input)
			if err != nil || !reflect.DeepEqual(got, fixture.Tokens) {
				t.Fatalf("Tokenize(%q)=%+v, want %+v: %v", fixture.Input, got, fixture.Tokens, err)
			}
		})
	}
	for _, fixture := range fixtures.Invalid {
		if fixture.Name == "" || seen[fixture.Name] {
			t.Fatal("fixture names must be unique and nonempty", fixture.Name)
		}
		seen[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			if _, err := Tokenize(fixture.Input); !errors.Is(err, ErrTextInvalid) {
				t.Fatalf("Tokenize(%q) accepted invalid input: %v", fixture.Input, err)
			}
		})
	}
}
