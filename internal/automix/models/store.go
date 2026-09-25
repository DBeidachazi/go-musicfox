package models

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Options 来自配置的模型设置。
type Options struct {
	// Dir 模型目录。
	Dir string
	// Level 下载到哪一级。
	Level Level
	// Mirrors 额外的镜像模板（含一个 %s 代表文件名），排在内置镜像之前。
	Mirrors []string
	// Python 用户自备的 Python（需装有 onnxruntime 与 numpy），优先于下载的运行时。
	// 没有 folia 运行时的平台（Linux arm64、Intel Mac）靠它跑 L2。
	Python string
	// OrtLibrary 用户自备的 onnxruntime 动态库，优先于下载的。
	OrtLibrary string
	// StemBackend auto | python | native。auto：有 Python 用 Python，否则进程内 ORT。
	StemBackend string
	// Threads 推理线程数，<=0 取逻辑核数的四分之一。
	Threads int
}

// Store 模型目录的状态与后台下载。
type Store struct {
	opts   Options
	client *http.Client

	mu          sync.Mutex
	downloading bool
	lastErr     map[string]error
}

// NewStore 构造。不做任何 I/O。
func NewStore(opts Options) *Store {
	if opts.StemBackend == "" {
		opts.StemBackend = "auto"
	}
	return &Store{
		opts: opts,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   20 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
		lastErr: map[string]error{},
	}
}

// Options 当前设置。
func (s *Store) Options() Options { return s.opts }

// Threads 推理线程数。一台机器的四分之一：该留给播放与界面的份额不应随机器大小变化。
func (s *Store) Threads() int {
	if s.opts.Threads > 0 {
		return s.opts.Threads
	}
	return max(1, runtime.NumCPU()/4)
}

func (s *Store) artifacts() []Artifact {
	var list []Artifact
	if s.opts.Level >= LevelBeat {
		list = append(list, beatThis(s.opts.Mirrors))
		if s.opts.OrtLibrary == "" {
			if a, _, ok := ortLibrary(); ok {
				list = append(list, a)
			}
		}
	}
	if s.opts.Level >= LevelFull {
		list = append(list, htdemucs(s.opts.Mirrors))
		if s.opts.Python == "" && s.opts.StemBackend != "native" {
			if a, ok := pythonRuntime(s.opts.Mirrors); ok {
				list = append(list, a)
			}
		}
	}
	return list
}

func (s *Store) path(parts ...string) string {
	return filepath.Join(append([]string{s.opts.Dir}, parts...)...)
}

// markerOf 校验通过后写下的标记，避免每次启动都重算 100MB 的哈希。
func (s *Store) markerOf(a Artifact) string {
	if a.Unpack != "" {
		return s.path(a.Unpack, ".verified")
	}
	return s.path(a.File + ".verified")
}

func fileIsRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// verified 该文件已下载并校验过。只看标记与大小，不重算哈希。
func (s *Store) verified(a Artifact) bool {
	marker, err := os.ReadFile(s.markerOf(a))
	if err != nil {
		return false
	}
	if a.SHA256 != "" && strings.TrimSpace(string(marker)) != a.SHA256 {
		return false
	}
	if a.Unpack != "" {
		return true
	}
	info, err := os.Stat(s.path(a.File))
	return err == nil && info.Mode().IsRegular() && (a.Bytes == 0 || info.Size() == a.Bytes)
}

// BeatThisModel Beat This! 模型路径；未就绪时 ok=false。
func (s *Store) BeatThisModel() (string, bool) {
	if s.opts.Level < LevelBeat {
		return "", false
	}
	a := beatThis(nil)
	return s.path(a.File), s.verified(a)
}

// HtdemucsModel htdemucs 模型路径；未就绪时 ok=false。
func (s *Store) HtdemucsModel() (string, bool) {
	if s.opts.Level < LevelFull {
		return "", false
	}
	a := htdemucs(nil)
	return s.path(a.File), s.verified(a)
}

// OrtLibrary onnxruntime 动态库路径。
func (s *Store) OrtLibrary() (string, bool) {
	if s.opts.OrtLibrary != "" {
		return s.opts.OrtLibrary, fileIsRegular(s.opts.OrtLibrary)
	}
	a, lib, ok := ortLibrary()
	if !ok {
		return "", false
	}
	path := s.path(a.Unpack, lib)
	return path, s.verified(a) && fileIsRegular(path)
}

// Python 分离用的解释器：用户自备的优先，其次是下载的 folia 运行时。
func (s *Store) Python() (string, bool) {
	if s.opts.Python != "" {
		return s.opts.Python, true
	}
	a, ok := pythonRuntime(nil)
	if !ok || !s.verified(a) {
		return "", false
	}
	exe := s.path(runtimeDir, "bin", "python3")
	if runtime.GOOS == "windows" {
		exe = s.path(runtimeDir, "python.exe")
	}
	return exe, fileIsRegular(exe)
}

// Status 一行人能读的状态，供日志使用。
func (s *Store) Status() string {
	var parts []string
	for _, a := range s.artifacts() {
		state := "missing"
		if s.verified(a) {
			state = "ready"
		} else if err := s.errOf(a.Name); err != nil {
			state = "failed: " + err.Error()
		}
		parts = append(parts, a.Name+"="+state)
	}
	if s.opts.Level >= LevelFull && s.opts.Python == "" {
		if _, ok := pythonRuntime(nil); !ok {
			parts = append(parts, "runtime=unavailable on "+Platform()+" (set automixPython, or stems run in-process)")
		}
	}
	if len(parts) == 0 {
		return "no models selected"
	}
	return strings.Join(parts, ", ")
}

func (s *Store) errOf(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr[name]
}

// EnsureAsync 在后台补齐所选级别缺的文件。重复调用无副作用；done 在全部结束时调用（可为 nil）。
func (s *Store) EnsureAsync(ctx context.Context, done func()) {
	s.mu.Lock()
	if s.downloading || s.opts.Level == LevelNone {
		s.mu.Unlock()
		return
	}
	s.downloading = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.downloading = false
			s.mu.Unlock()
			if done != nil {
				done()
			}
		}()
		if err := os.MkdirAll(s.opts.Dir, 0o755); err != nil {
			slog.Warn("automix models: cannot create directory", "dir", s.opts.Dir, "error", err)
			return
		}
		for _, a := range s.artifacts() {
			if s.verified(a) {
				continue
			}
			err := s.fetch(ctx, a)
			s.mu.Lock()
			s.lastErr[a.Name] = err
			s.mu.Unlock()
			if err != nil {
				slog.Warn("automix models: not installed", "name", a.Name, "error", err)
				if ctx.Err() != nil {
					return
				}
			}
		}
		slog.Info("automix models", "dir", s.opts.Dir, "status", s.Status())
	}()
}

// fetch 取得一个文件：已手动放入的先校验，否则依次试镜像。
func (s *Store) fetch(ctx context.Context, a Artifact) error {
	target := s.path(a.File)
	// 手动放进来的文件（或上次下载后没来得及写标记的）：先校验，不要重下。
	if fileIsRegular(target) {
		if sum, err := checkFile(target, a); err == nil {
			slog.Info("automix models: found a local copy", "name", a.Name, "sha256", sum)
			return s.install(a, target, sum)
		} else {
			slog.Warn("automix models: local copy rejected", "name", a.Name, "error", err)
		}
	}
	var errs []error
	for _, url := range a.URLs {
		sum, err := s.download(ctx, url, target, a)
		if err == nil {
			return s.install(a, target, sum)
		}
		errs = append(errs, fmt.Errorf("%s: %w", url, err))
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

func (s *Store) install(a Artifact, archive, sum string) error {
	if a.SHA256 == "" {
		slog.Warn("automix models: hash not pinned, verified by size and origin only",
			"name", a.Name, "file", a.File, "sha256", sum)
	}
	if a.Unpack != "" {
		dest := s.path(a.Unpack)
		if err := extract(archive, dest, a.Keep); err != nil {
			return fmt.Errorf("unpack %s: %w", a.File, err)
		}
		_ = os.Remove(archive)
	}
	return os.WriteFile(s.markerOf(a), []byte(sum+"\n"), 0o644)
}

func checkFile(path string, a Artifact) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	return verify(a, n, hex.EncodeToString(h.Sum(nil)))
}

func verify(a Artifact, n int64, sum string) (string, error) {
	if a.Bytes > 0 && n != a.Bytes {
		return sum, fmt.Errorf("%d bytes, expected %d", n, a.Bytes)
	}
	if a.SHA256 != "" && sum != a.SHA256 {
		return sum, fmt.Errorf("sha256 %s, expected %s", sum, a.SHA256)
	}
	return sum, nil
}

// progressWriter 每 10% 记一行日志。
type progressWriter struct {
	name        string
	total, done int64
	next        int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.total > 0 && p.done*10/p.total >= p.next {
		slog.Info("automix models: downloading", "name", p.name,
			"progress", fmt.Sprintf("%d%%", p.done*100/p.total), "mb", p.done>>20)
		p.next = p.done*10/p.total + 1
	}
	return len(b), nil
}

func (s *Store) download(ctx context.Context, url, target string, a Artifact) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "go-musicfox-automix")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if a.Bytes > 0 {
		if total > 0 && total != a.Bytes {
			return "", fmt.Errorf("server offers %d bytes, expected %d", total, a.Bytes)
		}
		total = a.Bytes
	}
	part := target + ".part"
	f, err := os.Create(part)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	slog.Info("automix models: downloading", "name", a.Name, "url", url)
	n, copyErr := io.Copy(io.MultiWriter(f, h, &progressWriter{name: a.Name, total: total}), resp.Body)
	closeErr := f.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(part)
		return "", copyErr
	}
	sum, err := verify(a, n, hex.EncodeToString(h.Sum(nil)))
	if err != nil {
		_ = os.Remove(part)
		return "", err
	}
	if err := os.Rename(part, target); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	return sum, nil
}

// extract 解包到 dest（先解到临时目录再换名）。keep 非空时只取匹配基名的普通文件并摊平；
// 否则保留目录结构，且若归档只有一个顶层目录则剥掉它。
func extract(archive, dest string, keep func(string) bool) error {
	tmp := dest + ".unpack"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	var err error
	switch {
	case strings.HasSuffix(archive, ".zip"):
		err = extractZip(archive, tmp, keep)
	case strings.HasSuffix(archive, ".tgz"), strings.HasSuffix(archive, ".tar.gz"):
		err = extractTgz(archive, tmp, keep)
	default:
		err = fmt.Errorf("unknown archive type")
	}
	if err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	root := tmp
	if keep == nil {
		if entries, e := os.ReadDir(tmp); e == nil && len(entries) == 1 && entries[0].IsDir() {
			root = filepath.Join(tmp, entries[0].Name())
		}
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(root, dest); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	_ = os.RemoveAll(tmp)
	return nil
}

// safeJoin 拒绝逃出目标目录的条目（zip-slip）。
func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	return filepath.Join(dir, clean), nil
}

func writeEntry(path string, mode os.FileMode, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	perm := mode.Perm() | 0o600
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// makeLink 只接受指向归档内部的相对链接。
func makeLink(dir, path, target string) error {
	if filepath.IsAbs(target) {
		return fmt.Errorf("absolute symlink %q", target)
	}
	resolved := filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
	if rel, err := filepath.Rel(dir, resolved); err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("symlink %q escapes the archive", target)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.Symlink(filepath.FromSlash(target), path)
}

func extractZip(archive, dir string, keep func(string) bool) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		mode := f.Mode()
		base := filepath.Base(filepath.FromSlash(f.Name))
		if keep != nil {
			if !mode.IsRegular() || !keep(base) {
				continue
			}
			if err := copyZipEntry(f, filepath.Join(dir, base), mode); err != nil {
				return err
			}
			continue
		}
		path, err := safeJoin(dir, f.Name)
		if err != nil {
			return err
		}
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			_ = rc.Close()
			if err != nil {
				return err
			}
			if runtime.GOOS == "windows" {
				continue
			}
			if err := makeLink(dir, path, string(target)); err != nil {
				return err
			}
		default:
			if err := copyZipEntry(f, path, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyZipEntry(f *zip.File, path string, mode os.FileMode) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeEntry(path, mode, rc)
}

func extractTgz(archive, dir string, keep func(string) bool) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		base := filepath.Base(filepath.FromSlash(h.Name))
		if keep != nil {
			if h.Typeflag != tar.TypeReg || !keep(base) {
				continue
			}
			if err := writeEntry(filepath.Join(dir, base), h.FileInfo().Mode(), tr); err != nil {
				return err
			}
			continue
		}
		path, err := safeJoin(dir, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeEntry(path, h.FileInfo().Mode(), tr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := makeLink(dir, path, h.Linkname); err != nil {
				return err
			}
		}
	}
}
