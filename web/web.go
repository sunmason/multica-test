// Package web embeds the static frontend served by the API server.
package web

import "embed"

//go:embed index.html app.js style.css
var Files embed.FS
