// Package api embeds the OpenAPI contract so the server can serve it and
// tests can check the handlers against it.
package api

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
