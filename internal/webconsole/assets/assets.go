// Package assets provides the styling and branding available before sign-in.
package assets

import "embed"

//go:embed access.css theme.css lifebuoy.png
var Files embed.FS
