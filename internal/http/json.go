package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"unicode/utf8"
)

// decodeJSON rejects oversized, ambiguous, unknown-field, and non-object inputs.
// Case-sensitive root keys follow OpenAPI instead of encoding/json's case folding.
func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || (parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8")) {
		problem(w, 415, "UNSUPPORTED_MEDIA_TYPE", "Send application/json with UTF-8 encoding.")
		return false
	}
	body := http.MaxBytesReader(w, r.Body, 16<<10)
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			problem(w, 413, "BODY_TOO_LARGE", "The JSON body exceeds 16 KiB.")
		} else {
			problem(w, 400, "INVALID_JSON", "The request body could not be read.")
		}
		return false
	}
	if !utf8.Valid(data) {
		problem(w, 400, "INVALID_JSON", "The JSON body must contain valid UTF-8.")
		return false
	}
	if err := unambiguousJSON(data); err != nil {
		problem(w, 400, "INVALID_JSON", "Send one JSON object without duplicate keys or excessive nesting.")
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		problem(w, 400, "INVALID_JSON", "Send a JSON object.")
		return false
	}
	known := map[string]bool{}
	typeOf := reflect.TypeOf(destination).Elem()
	for i := 0; i < typeOf.NumField(); i++ {
		known[strings.Split(typeOf.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for field, value := range fields {
		if !known[field] {
			problem(w, 400, "INVALID_JSON", "The JSON object contains an unknown field.")
			return false
		}
		// Implemented input contracts have no nullable fields. Omit an optional
		// field instead; null must not silently become false or an absent patch.
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			problem(w, 400, "INVALID_JSON", "The JSON fields must not be null.")
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		problem(w, 400, "INVALID_JSON", "The JSON fields have invalid types or names.")
		return false
	}
	return true
}

func unambiguousJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 16 {
			return errors.New("too deeply nested")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delimiter != '{' && delimiter != '[' {
			return errors.New("unexpected delimiter")
		}
		keys := map[string]bool{}
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("duplicate object key")
				}
				keys[name] = true
			}
			if err := value(depth + 1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
