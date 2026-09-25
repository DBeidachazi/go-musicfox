// Package models 管理 automix 的两个可选神经网络：拍点检测 Beat This!（L1）与音轨分离 htdemucs（L2）。
//
// 两者都不进安装包，按配置在后台下载到模型目录（默认 <data>/automix/models），也可以手动放进去。
// 模型文件与 folia-major 所用的完全一致（清单与哈希抄自其 shared/modelManifest.json），
// 所以同一份文件可以在两个播放器之间共用。
package models

import (
	"fmt"
	"runtime"
)

// Level 用户选择下载到哪一级。
type Level int

const (
	// LevelNone 不下载任何模型，纯 DSP 估计（L0）。
	LevelNone Level = iota
	// LevelBeat 下载 Beat This!（83MB）与 onnxruntime 动态库：拍点、重拍、拍号由模型给出。
	LevelBeat
	// LevelFull 再下载 htdemucs（109MB）与分离所用的 Python 运行时：人声退场、鼓与贝斯换手。
	LevelFull
)

// ParseLevel 解析配置值：none | beat | full。
func ParseLevel(s string) Level {
	switch s {
	case "beat", "l1":
		return LevelBeat
	case "full", "stems", "l2":
		return LevelFull
	default:
		return LevelNone
	}
}

func (l Level) String() string {
	switch l {
	case LevelBeat:
		return "beat"
	case LevelFull:
		return "full"
	default:
		return "none"
	}
}

// Artifact 一个可下载的文件。
type Artifact struct {
	Name string
	// File 下载到模型目录下的文件名。
	File string
	// Bytes 期望大小，0 表示不校验。
	Bytes int64
	// SHA256 期望哈希，空表示未钉住（只校验大小，并在日志里打印算出的哈希）。
	//
	// 哈希才是清单的重点：被截断或被镜像替换的 ONNX 不会显式失败，它会加载成功，
	// 然后自信地给出错误答案。
	SHA256 string
	// URLs 依次尝试的地址，第一个校验通过的胜出。
	URLs []string
	// Unpack 非空时这是一个归档（按扩展名 .zip / .tgz），解到模型目录下的这个子目录。
	Unpack string
	// Keep 解包时只保留匹配的条目（按基名）；nil 表示全部保留。
	Keep func(base string) bool
	// Enables 这个文件为哪一级服务。
	Enables Level
	License string
}

// folia 的镜像，依次尝试。hf-mirror 排第一是因为它是中国大陆唯一可达的路线。
var foliaMirrors = []string{
	"https://hf-mirror.com/HUAI4236/folia-models/resolve/main/%s",
	"https://huggingface.co/HUAI4236/folia-models/resolve/main/%s",
	"https://github.com/AZURE-HUAI/folia-models/releases/download/weights-v1/%s",
}

func mirrored(extra []string, file string) []string {
	var urls []string
	for _, m := range extra {
		urls = append(urls, fmt.Sprintf(m, file))
	}
	for _, m := range foliaMirrors {
		urls = append(urls, fmt.Sprintf(m, file))
	}
	return urls
}

// BeatThisFile / HtdemucsFile 模型目录下的文件名。
const (
	BeatThisFile = "beat_this.onnx"
	HtdemucsFile = "htdemucs.onnx"
	runtimeDir   = "runtime"
	ortDir       = "onnxruntime"
)

func beatThis(extra []string) Artifact {
	return Artifact{
		Name: "beat_this", File: BeatThisFile, Bytes: 83077778,
		SHA256:  "c5c1466e08abdb03fdeb50668a06f244b787d564c212490482231a9cfbe9ccbd",
		URLs:    mirrored(extra, BeatThisFile),
		Enables: LevelBeat, License: "MIT (CPJKU Beat This!, community ONNX export of final0)",
	}
}

func htdemucs(extra []string) Artifact {
	return Artifact{
		Name: "htdemucs", File: HtdemucsFile, Bytes: 108644650,
		SHA256:  "099b5be76c1f6922124d07f850250f39d1f33f254a0b8cc90f4ec0dfd0912329",
		URLs:    mirrored(extra, HtdemucsFile),
		Enables: LevelFull, License: "MIT (Meta htdemucs, fp16, transparent re-export, segment halved by folia-major)",
	}
}

// Platform GOOS-GOARCH，与下面几张表的键一致。
func Platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// folia 的裁剪版 CPython 3.11 + onnxruntime 1.29 + numpy，只为拨动 enable_mem_reuse 这一个开关：
// 它把一次分离从 2.5GB 压到 500MB 以下，而只有 Python 绑定能设置它。
var foliaRuntimes = map[string]struct {
	file  string
	bytes int64
	sha   string
}{
	"windows-amd64": {"folia-runtime-win32-x64.zip", 35633855, "50a5829ac928071ab2eecaf2667e5dab9c3af2b4b44fb5bb4b7acfb9863cdacd"},
	"darwin-arm64":  {"folia-runtime-darwin-arm64.zip", 44380755, "f8313150254d699396b4ff4c9289bdd2864ba3268bd137e5aeb17963be318cde"},
	"linux-amd64":   {"folia-runtime-linux-x64.zip", 60509680, "cf1347f5621fed74f8b2dea243ea1365824ac77ab52ddc2183ca8a77180b60da"},
}

func pythonRuntime(extra []string) (Artifact, bool) {
	rt, ok := foliaRuntimes[Platform()]
	if !ok {
		return Artifact{}, false
	}
	return Artifact{
		Name: "runtime", File: rt.file, Bytes: rt.bytes, SHA256: rt.sha,
		URLs: mirrored(extra, rt.file), Unpack: runtimeDir,
		Enables: LevelFull, License: "PSF-2.0 (CPython), MIT (onnxruntime), BSD-3-Clause (numpy)",
	}, true
}

// onnxruntime 官方发布的 CPU 包，只取出动态库。版本与 folia 运行时里的一致；
// Intel Mac 从 1.24 起不再有官方包，停在 1.23.2。
//
// Windows x64 包的 sha256 已按官方 v1.29.0 发布资产钉住；其余平台尚未钉住
// （见 Artifact.SHA256），下载时只校验来源并在日志里打印哈希，钉住后填进来即可。
var ortPackages = map[string]struct{ version, asset, lib, sha string }{
	"linux-amd64":   {"1.29.0", "onnxruntime-linux-x64-1.29.0.tgz", "libonnxruntime.so.1.29.0", ""},
	"linux-arm64":   {"1.29.0", "onnxruntime-linux-aarch64-1.29.0.tgz", "libonnxruntime.so.1.29.0", ""},
	"darwin-arm64":  {"1.29.0", "onnxruntime-osx-arm64-1.29.0.tgz", "libonnxruntime.1.29.0.dylib", ""},
	"darwin-amd64":  {"1.23.2", "onnxruntime-osx-x86_64-1.23.2.tgz", "libonnxruntime.1.23.2.dylib", ""},
	"windows-amd64": {"1.29.0", "onnxruntime-win-x64-1.29.0.zip", "onnxruntime.dll", "c9b4b7086b529ad814f428c1bad028e20a25d7dc0699836775faace4ab5b78b2"},
	"windows-arm64": {"1.29.0", "onnxruntime-win-arm64-1.29.0.zip", "onnxruntime.dll", ""},
}

func ortLibrary() (Artifact, string, bool) {
	pkg, ok := ortPackages[Platform()]
	if !ok {
		return Artifact{}, "", false
	}
	return Artifact{
		Name: "onnxruntime", File: pkg.asset, SHA256: pkg.sha,
		URLs:   []string{fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/%s", pkg.version, pkg.asset)},
		Unpack: ortDir,
		// 只要动态库本身；Windows 上 providers_shared 与主库放在一起。
		Keep: func(base string) bool {
			return base == pkg.lib || base == "onnxruntime_providers_shared.dll"
		},
		Enables: LevelBeat, License: "MIT (Microsoft onnxruntime " + pkg.version + ")",
	}, pkg.lib, true
}
