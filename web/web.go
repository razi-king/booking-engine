// Package web embeds the demo seat-map page into the binary.
package web

import "embed"

// Files holds index.html. go:embed copies it into the compiled program, so
// the server needs no files on disk.
//
//go:embed index.html
var Files embed.FS
