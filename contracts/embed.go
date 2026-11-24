// Package contracts embeds the public API description for conformance tests.
package contracts

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte

// SearchUnicodeTables is the immutable shared Unicode search profile.
//
//go:embed search-normalization-tables.json
var SearchUnicodeTables string
