// Package templates holds the Dockerfiles cd builds projects with; the project's own files
// never decide how its container runs.
package templates

import "embed"

//go:embed node/Dockerfile.tmpl static/Dockerfile.tmpl
var FS embed.FS
