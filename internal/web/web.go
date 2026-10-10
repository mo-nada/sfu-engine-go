// Package web serves the browser pages: the demo client and the API
// documentation viewers. The pages are development aids, not production UI.
package web

import (
	_ "embed"
	"net/http"
)

var (
	//go:embed index.html
	indexHTML []byte
	//go:embed docs.html
	docsHTML []byte
	//go:embed signaling-docs.html
	signalingDocsHTML []byte
)

// Handler serves the demo page.
func Handler() http.Handler { return serve("text/html; charset=utf-8", indexHTML) }

// Docs serves the Swagger UI page for the HTTP API (/api/openapi.yaml).
func Docs() http.Handler { return serve("text/html; charset=utf-8", docsHTML) }

// SignalingDocs serves the AsyncAPI viewer page for the signaling protocol (/api/asyncapi.yaml).
func SignalingDocs() http.Handler { return serve("text/html; charset=utf-8", signalingDocsHTML) }

// File serves fixed content with the given content type.
func File(contentType string, content []byte) http.Handler { return serve(contentType, content) }

func serve(contentType string, content []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(content)
	})
}
