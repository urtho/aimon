// Package web embeds the dashboard's templates and static assets so the final
// binary is fully self-contained (important for the scratch image — there is no
// filesystem to read from at runtime).
package web

import "embed"

//go:embed templates/*.html static/*
var FS embed.FS
