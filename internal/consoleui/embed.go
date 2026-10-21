// Package consoleui contains the reproducible Vite production build.
package consoleui

import "embed"

//go:embed dist
var Assets embed.FS
