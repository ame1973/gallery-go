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
// Field names are case-sensitive at every depth, following the OpenAPI contract.
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
	if !exactJSONFields(data, reflect.TypeOf(destination).Elem()) {
		problem(w, 400, "INVALID_JSON", "Use exact known field names and non-null values, including in nested objects.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		problem(w, 400, "INVALID_JSON", "The JSON fields have invalid types or names.")
		return false
	}
	return true
}

// All current request fields are non-nullable. encoding/json otherwise silently
// accepts null for scalar/struct fields and case-folds nested names. Validate the
// declared shape recursively before decoding; future nullable fields need an
// intentional contract/decoder extension, not a global null-to-zero fallback.
func exactJSONFields(data []byte, kind reflect.Type) bool {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return false
	}
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	switch kind.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			return false
		}
		known := make(map[string]reflect.Type, kind.NumField())
		for index := 0; index < kind.NumField(); index++ {
			field := kind.Field(index)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if field.PkgPath == "" && name != "" && name != "-" {
				known[name] = field.Type
			}
		}
		for name, value := range fields {
			field, ok := known[name]
			if !ok || !exactJSONFields(value, field) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if json.Unmarshal(data, &values) != nil {
			return false
		}
		for _, value := range values {
			if !exactJSONFields(value, kind.Elem()) {
				return false
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if kind.Key().Kind() != reflect.String || json.Unmarshal(data, &values) != nil {
			return false
		}
		for _, value := range values {
			if !exactJSONFields(value, kind.Elem()) {
				return false
			}
		}
	}
	return true
}

func requireEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body != nil {
		data, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(data) != 0 {
			problem(w, 400, "INVALID_BODY", "This command does not accept a request body.")
			return false
		}
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
