//go:build windows && (amd64 || arm64)

package ort

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// pathChar ORTCHAR_T：Windows 上是 wchar_t。
type pathChar = uint16

func toPathChars(s string) []pathChar {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return []uint16{0}
	}
	return u
}

func openLibrary(path string) (uintptr, error) {
	// 带上库所在目录，使 onnxruntime.dll 旁边的依赖（如 onnxruntime_providers_shared.dll）能被找到。
	h, err := windows.LoadLibraryEx(path, 0,
		windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS)
	if err != nil {
		h, err = windows.LoadLibrary(filepath.Clean(path))
	}
	return uintptr(h), err
}

func lookup(handle uintptr, name string) (uintptr, error) {
	return windows.GetProcAddress(windows.Handle(handle), name)
}
