// Package web holds the embedded interface.
//
// The files are embedded rather than served from disk so that idxa stays a
// single binary: someone can download one file, run "idxa serve", and have a
// working interface without a runtime, a package manager or a build step.
package web

import "embed"

// Assets holds the built interface. There is no build step: the files under
// dist are the sources, hand written so that changing a colour does not
// require a toolchain.
//
//go:embed dist
var Assets embed.FS
