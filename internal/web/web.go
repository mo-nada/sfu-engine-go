// Package web serves the browser demo page. It is a development aid for
// trying the SFU with real browsers, not a production client.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// Handler serves the demo page.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
}
