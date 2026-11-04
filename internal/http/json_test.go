package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

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
