// Package docs embeds this directory's own markdown files so the web UI
// can show them directly (the new Help page, internal/web/templates/help.html)
// without a second, hand-copied version to keep in sync. `go:embed`
// can't reach outside the directory containing the file that declares
// it, so this package exists specifically to sit next to API.md itself —
// internal/web imports it rather than duplicating the content.
package docs

import _ "embed"

//go:embed API.md
var APIMarkdown string
