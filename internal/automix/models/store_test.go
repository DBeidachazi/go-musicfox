package models

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func serve(t *testing.T, files map[string][]byte) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchFallsThroughMirrorsUntilHashMatches(t *testing.T) {
	good := []byte("the real weights")
	srv := serve(t, map[string][]byte{
		"/bad/m.onnx":  []byte("a mirror that swapped the file"),
		"/good/m.onnx": good,
	})
	dir := t.TempDir()
	s := NewStore(Options{Dir: dir})
	a := Artifact{Name: "m", File: "m.onnx", Bytes: int64(len(good)), SHA256: sum(good),
		URLs: []string{srv.URL + "/missing/m.onnx", srv.URL + "/bad/m.onnx", srv.URL + "/good/m.onnx"}}
	if err := s.fetch(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "m.onnx"))
	if !bytes.Equal(got, good) || !s.verified(a) {
		t.Fatalf("got %q verified=%v", got, s.verified(a))
	}
	if _, err := os.Stat(filepath.Join(dir, "m.onnx.part")); !os.IsNotExist(err) {
		t.Error("partial file left behind")
	}
}

func TestFetchRejectsEveryWrongCopy(t *testing.T) {
	srv := serve(t, map[string][]byte{"/m.onnx": []byte("truncated")})
	s := NewStore(Options{Dir: t.TempDir()})
	a := Artifact{Name: "m", File: "m.onnx", SHA256: sum([]byte("expected")), URLs: []string{srv.URL + "/m.onnx"}}
	if err := s.fetch(context.Background(), a); err == nil || s.verified(a) {
		t.Fatal("accepted a file whose hash does not match")
	}
}

func TestLocalCopyIsVerifiedNotRedownloaded(t *testing.T) {
	good := []byte("carried by hand")
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "m.onnx"), good, 0o644)
	s := NewStore(Options{Dir: dir})
	a := Artifact{Name: "m", File: "m.onnx", SHA256: sum(good), URLs: []string{"http://127.0.0.1:1/unreachable"}}
	if err := s.fetch(context.Background(), a); err != nil || !s.verified(a) {
		t.Fatal(err)
	}
}

func tgz(t *testing.T, entries []tar.Header, bodies [][]byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i, h := range entries {
		h.Size = int64(len(bodies[i]))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(bodies[i])
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestTgzKeepFlattensOnlyTheLibrary(t *testing.T) {
	lib := []byte("\x7fELF pretend")
	archive := tgz(t, []tar.Header{
		{Name: "onnxruntime-linux-x64-1.29.0/include/onnxruntime_c_api.h", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "onnxruntime-linux-x64-1.29.0/lib/libonnxruntime.so.1.29.0", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "onnxruntime-linux-x64-1.29.0/lib/libonnxruntime.so", Typeflag: tar.TypeSymlink, Linkname: "libonnxruntime.so.1"},
	}, [][]byte{[]byte("header"), lib, nil})
	srv := serve(t, map[string][]byte{"/o.tgz": archive})
	dir := t.TempDir()
	s := NewStore(Options{Dir: dir})
	a := Artifact{Name: "onnxruntime", File: "o.tgz", URLs: []string{srv.URL + "/o.tgz"}, Unpack: "onnxruntime",
		Keep: func(b string) bool { return b == "libonnxruntime.so.1.29.0" }}
	if err := s.fetch(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "onnxruntime"))
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 { // the library and the .verified marker
		t.Fatalf("unpacked %v", names)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "onnxruntime", "libonnxruntime.so.1.29.0"))
	if !bytes.Equal(got, lib) || !s.verified(a) {
		t.Fatal("library not installed")
	}
	if _, err := os.Stat(filepath.Join(dir, "o.tgz")); !os.IsNotExist(err) {
		t.Error("archive kept after unpacking")
	}
}

func zipOf(t *testing.T, files map[string]string, links map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		w, _ := zw.CreateHeader(h)
		_, _ = w.Write([]byte(body))
	}
	for name, target := range links {
		h := &zip.FileHeader{Name: name}
		h.SetMode(os.ModeSymlink | 0o777)
		w, _ := zw.CreateHeader(h)
		_, _ = w.Write([]byte(target))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestZipStripsSingleTopDirectoryAndKeepsLinks(t *testing.T) {
	archive := zipOf(t, map[string]string{
		"runtime/bin/python3.11":  "#!python",
		"runtime/lib/numpy/x.py":  "",
		"runtime/lib/onnxruntime": "",
	}, map[string]string{"runtime/bin/python3": "python3.11"})
	path := filepath.Join(t.TempDir(), "rt.zip")
	_ = os.WriteFile(path, archive, 0o644)
	dest := filepath.Join(t.TempDir(), "runtime")
	if err := extract(path, dest, nil); err != nil {
		t.Fatal(err)
	}
	if !fileIsRegular(filepath.Join(dest, "bin", "python3.11")) {
		t.Fatal("top directory not stripped")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dest, "bin", "python3"))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("link or exec bit lost: %v %v", info, err)
		}
	}
}

func TestZipSlipIsRefused(t *testing.T) {
	for _, archive := range [][]byte{
		zipOf(t, map[string]string{"../../evil": "x"}, nil),
		zipOf(t, map[string]string{"ok/a": "x"}, map[string]string{"ok/l": "../../../etc/passwd"}),
	} {
		path := filepath.Join(t.TempDir(), "a.zip")
		_ = os.WriteFile(path, archive, 0o644)
		if err := extract(path, filepath.Join(t.TempDir(), "out"), nil); err == nil {
			t.Error("unsafe archive accepted")
		}
	}
}

func TestLevelSelectsArtifacts(t *testing.T) {
	names := func(o Options) map[string]bool {
		m := map[string]bool{}
		for _, a := range NewStore(o).artifacts() {
			m[a.Name] = true
		}
		return m
	}
	if n := names(Options{Level: LevelNone}); len(n) != 0 {
		t.Error(n)
	}
	beat := names(Options{Level: LevelBeat})
	if !beat["beat_this"] || beat["htdemucs"] {
		t.Error(beat)
	}
	full := names(Options{Level: LevelFull, Python: "/usr/bin/python3", OrtLibrary: "/x/lib.so"})
	if !full["htdemucs"] || full["runtime"] || full["onnxruntime"] {
		t.Error(full)
	}
	if ParseLevel("full") != LevelFull || ParseLevel("beat") != LevelBeat || ParseLevel("nonsense") != LevelNone {
		t.Error("ParseLevel")
	}
}
