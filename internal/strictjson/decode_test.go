package strictjson

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnambiguousJSON(t *testing.T) {
	for _, data := range []string{`{"a":{"b":1,"b":2}}`, `{"a":1} trailing`, `{"a":[1,]}`, strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18)} {
		if unambiguousJSON([]byte(data)) == nil {
			t.Fatalf("ambiguous JSON accepted: %s", data)
		}
	}
	for _, data := range []string{`{"a":[{"b":1},{"b":2}],"c":true}`, `{"x":null}`, `{"num":123456789012345678901234567890}`} {
		if err := unambiguousJSON([]byte(data)); err != nil {
			t.Fatalf("valid JSON %s: %v", data, err)
		}
	}
}

type object struct {
	Name  string `json:"name"`
	Child []struct {
		Width int `json:"width"`
	} `json:"child"`
	Values    map[string]string `json:"values"`
	Nullable  *string           `json:"nullable" nullable:"true"`
	Optional  *bool             `json:"optional"`
	WrongTag  string            `json:"wrong_tag" nullable:"true"`
	unexposed string
}

func TestDecodePreservesExactRecursiveHTTPAndCodecTrustBoundaries(t *testing.T) {
	for _, data := range []string{
		`{}`, `{"name":"x","child":[{"width":1}],"values":{"v":"x"},"nullable":null}`,
		`{"nullable":"x","optional":false}`, `{"child":[],"values":{}}`,
	} {
		var target object
		if err := Decode([]byte(data), &target); err != nil {
			t.Fatal("declared object rejected", data, err)
		}
	}
	for _, data := range []string{
		`null`, `[]`, `"x"`, `1`, `{} {}`, `{"name":"x","name":"y"}`,
		`{"name":"x","name":"y"}`, `{"Name":"x"}`, `{"unknown":true}`,
		`{"name":null}`, `{"child":null}`, `{"child":[null]}`, `{"child":[{"Width":1}]}`,
		`{"child":[{"width":null}]}`, `{"child":[{"width":1,"width":2}]}`,
		`{"values":null}`, `{"values":{"x":null}}`, `{"optional":null}`, `{"wrong_tag":null}`,
		`{"unexposed":"x"}`, `{"name":1}`, "{\"name\":\"\xff\"}",
		strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18),
	} {
		var target object
		if err := Decode([]byte(data), &target); err == nil {
			t.Fatal("ambiguous, undeclared or null-to-zero JSON accepted", data)
		}
	}
	for _, target := range []any{nil, object{}, (*object)(nil)} {
		if err := Decode([]byte(`{}`), target); err == nil {
			t.Fatal("invalid destination accepted", target)
		}
	}
}

func FuzzStrictObjectDecode(f *testing.F) {
	for _, data := range []string{`{}`, `{"name":"x"}`, `{"child":[{"width":1}]}`, `{"nullable":null}`} {
		f.Add([]byte(data))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		var target object
		if Decode(data, &target) == nil {
			if !json.Valid(data) {
				t.Fatal("invalid JSON accepted")
			}
		}
	})
}
