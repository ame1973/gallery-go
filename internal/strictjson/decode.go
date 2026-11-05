// Package strictjson validates bounded untrusted objects without encoding/json's
// duplicate-key, case-folding or scalar-null fallbacks. Callers own byte limits.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid, ambiguous or uncontracted JSON object")

// Decode accepts one object, exact declared field names and explicitly nullable
// pointer fields only. Missing fields remain the caller's semantic validation duty.
func Decode(data []byte, destination any) error {
	kind := reflect.TypeOf(destination)
	if kind == nil || kind.Kind() != reflect.Pointer || reflect.ValueOf(destination).IsNil() || !utf8.Valid(data) {
		return ErrInvalid
	}
	if err := unambiguousJSON(data); err != nil {
		return ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil || !exactJSONFields(data, kind.Elem()) {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

// encoding/json silently accepts null for scalar/struct fields and case-folds
// nested names. Validate declared shapes recursively. Only an explicitly tagged
// nullable pointer field accepts null; all other null-to-zero fallbacks stay denied.
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
		known := make(map[string]reflect.StructField, kind.NumField())
		for index := 0; index < kind.NumField(); index++ {
			field := kind.Field(index)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if field.PkgPath == "" && name != "" && name != "-" {
				known[name] = field
			}
		}
		for name, value := range fields {
			field, ok := known[name]
			if !ok {
				return false
			}
			if field.Type.Kind() == reflect.Pointer && field.Tag.Get("nullable") == "true" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			if !exactJSONFields(value, field.Type) {
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
