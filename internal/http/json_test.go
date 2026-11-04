package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNullablePointerFieldsRequireExplicitLocalOptIn(t *testing.T) {
	type item struct {
		ID *string `json:"id" nullable:"true"`
	}
	type request struct {
		ID     *string `json:"id" nullable:"true"`
		Strict *string `json:"strict"`
		Scalar string  `json:"scalar" nullable:"true"`
		Nested *item   `json:"nested"`
		Items  []item  `json:"items"`
	}
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"id":null}`, true}, {`{"id":"known"}`, true}, {`{"nested":{"id":null}}`, true}, {`{"items":[{"id":null}]}`, true},
		{`null`, false}, {`{"strict":null}`, false}, {`{"scalar":null}`, false}, {`{"nested":null}`, false}, {`{"items":[null]}`, false},
		{`{"ID":null}`, false}, {`{"id":{}}`, false}, {`{"id":true}`, false}, {`{"nested":{"ID":null}}`, false},
	} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(test.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		var input request
		if valid := decodeJSON(w, r, &input); valid != test.valid {
			t.Fatal(test.body, valid, w.Body.String())
		}
	}
}

func TestNestedRequestFieldsAreExactNonNullAndUnambiguous(t *testing.T) {
	type mapping struct {
		AlbumDepth  int `json:"album_depth"`
		PersonDepth int `json:"person_depth"`
	}
	type request struct {
		Mapping *mapping           `json:"mapping"`
		Items   []mapping          `json:"items"`
		Options map[string]mapping `json:"options"`
	}
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"mapping":{"album_depth":2,"person_depth":1}}`, true},
		{`{"items":[{"album_depth":0}]}`, true},
		{`{"options":{"named":{"album_depth":1}}}`, true},
		{`{"mapping":{"Album_Depth":2}}`, false},
		{`{"mapping":{"root_path":"/private"}}`, false},
		{`{"mapping":{"album_depth":null}}`, false},
		{`{"mapping":{"person_depth":null}}`, false},
		{`{"mapping":null}`, false},
		{`{"mapping":{"album_depth":1,"album_depth":2}}`, false},
		{`{"items":[{"Album_Depth":2}]}`, false},
		{`{"items":[null]}`, false},
		{`{"options":{"named":{"Album_Depth":2}}}`, false},
		{`{"options":{"named":null}}`, false},
	} {
		t.Run(test.body, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", strings.NewReader(test.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			var input request
			if got := decodeJSON(w, r, &input); got != test.valid {
				t.Fatal("valid=", got, "want=", test.valid, w.Body.String())
			}
			if !test.valid && w.Code != 400 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}
