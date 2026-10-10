// Package api embeds the API description documents so the server can serve
// them: openapi.yaml (HTTP endpoints) and asyncapi.yaml (WebSocket signaling).
// Keep them in sync with the code; see platform-context.md.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3.1 document for the HTTP endpoints.
//
//go:embed openapi.yaml
var OpenAPI []byte

// AsyncAPI is the AsyncAPI 3.0 document for the WebSocket signaling protocol.
//
//go:embed asyncapi.yaml
var AsyncAPI []byte
