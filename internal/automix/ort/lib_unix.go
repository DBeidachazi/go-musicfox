//go:build (linux || darwin) && (amd64 || arm64)

package ort

import "github.com/ebitengine/purego"

// pathChar ORTCHAR_T：非 Windows 上是 char。
type pathChar = byte

func toPathChars(s string) []pathChar { return append([]byte(s), 0) }

func openLibrary(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

func lookup(handle uintptr, name string) (uintptr, error) { return purego.Dlsym(handle, name) }
