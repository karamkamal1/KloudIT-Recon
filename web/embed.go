// Package web embeds the browser client.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// FS returns the static client files rooted at static/.
func FS() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
