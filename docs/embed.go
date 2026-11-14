// Package docs embeds the user guide so the app can show it in-app.
package docs

import _ "embed"

// Guide is the Vietnamese user guide (Markdown).
//
//go:embed HUONG-DAN.md
var Guide string
