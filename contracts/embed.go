// Package contracts embeds the public API description for conformance tests.
package contracts

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
