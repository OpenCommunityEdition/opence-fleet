// Package static is the pages' files, embedded in the binary: the built
// stylesheet (app.css, from internal/view/style.css by tools/generate.sh),
// the islands (app.js), htmx and its SSE extension (vendor/), the icons
// and the favicon.
package static

import "embed"

// Files is the static files.
//
//go:embed app.css app.js favicon.svg icons.svg vendor
var Files embed.FS
