//go:build (linux || darwin || windows) && (amd64 || arm64)

// Package ort 是 ONNX Runtime C API 的最小绑定，只覆盖 automix 两个模型用到的部分。
//
// 不用 cgo：动态库在运行时按需下载，用 purego 加载并通过 OrtApi 函数指针表调用，
// 因此构建不增加任何原生依赖，没下载模型的用户也不会加载它。
//
// 函数在 OrtApi 结构体中的下标取自 onnxruntime_c_api.h（该结构体只追加不改动），
// 请求的 API 版本为 apiVersion，即 ORT >= 1.17 的动态库都可用。
package ort

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// apiVersion 请求的 OrtApi 版本。这里用到的函数都早于 1.17。
const apiVersion = 17

// OrtApi 函数指针表中的下标。
const (
	idxGetErrorMessage                = 2
	idxCreateEnv                      = 3
	idxCreateSession                  = 7
	idxRun                            = 9
	idxCreateSessionOptions           = 10
	idxSetSessionExecutionMode        = 13
	idxDisableMemPattern              = 17
	idxDisableCpuMemArena             = 19
	idxSetSessionGraphOptimization    = 23
	idxSetIntraOpNumThreads           = 24
	idxSetInterOpNumThreads           = 25
	idxSessionGetInputCount           = 30
	idxSessionGetInputTypeInfo        = 33
	idxSessionGetInputName            = 36
	idxSessionGetOutputName           = 37
	idxCreateTensorAsOrtValue         = 48
	idxGetTensorMutableData           = 51
	idxCastTypeInfoToTensorInfo       = 55
	idxGetDimensionsCount             = 61
	idxGetDimensions                  = 62
	idxGetTensorShapeElementCount     = 64
	idxGetTensorTypeAndShape          = 65
	idxAllocatorFree                  = 76
	idxGetAllocatorWithDefaultOptions = 78
	idxReleaseStatus                  = 93
	idxReleaseSession                 = 95
	idxReleaseValue                   = 96
	idxReleaseTypeInfo                = 98
	idxReleaseTensorTypeAndShapeInfo  = 99
	idxReleaseSessionOptions          = 100
	idxAddSessionConfigEntry          = 130
)

const (
	loggingLevelWarning = 2
	tensorFloat         = 1
	executionSequential = 0
	graphOptimizeAll    = 99
)

// 所有 OrtApi 函数都返回 OrtStatus*（nil 即成功），除了少数 Release/GetErrorMessage。
type api struct {
	getErrorMessage            func(status uintptr) string
	createEnv                  func(level int32, logID string, out *uintptr) uintptr
	createSession              func(env uintptr, path *pathChar, options uintptr, out *uintptr) uintptr
	run                        func(session, runOptions uintptr, inNames **byte, inputs *uintptr, inLen uintptr, outNames **byte, outLen uintptr, outputs *uintptr) uintptr
	createSessionOptions       func(out *uintptr) uintptr
	setSessionExecutionMode    func(options uintptr, mode int32) uintptr
	disableMemPattern          func(options uintptr) uintptr
	disableCpuMemArena         func(options uintptr) uintptr
	setGraphOptimization       func(options uintptr, level int32) uintptr
	setIntraOpNumThreads       func(options uintptr, n int32) uintptr
	setInterOpNumThreads       func(options uintptr, n int32) uintptr
	sessionGetInputCount       func(session uintptr, out *uintptr) uintptr
	sessionGetInputTypeInfo    func(session, index uintptr, out *uintptr) uintptr
	sessionGetInputName        func(session, index, allocator uintptr, out *uintptr) uintptr
	sessionGetOutputName       func(session, index, allocator uintptr, out *uintptr) uintptr
	createTensorAsOrtValue     func(allocator uintptr, shape *int64, shapeLen uintptr, elem int32, out *uintptr) uintptr
	getTensorMutableData       func(value uintptr, out *uintptr) uintptr
	castTypeInfoToTensorInfo   func(typeInfo uintptr, out *uintptr) uintptr
	getDimensionsCount         func(info uintptr, out *uintptr) uintptr
	getDimensions              func(info uintptr, dims *int64, n uintptr) uintptr
	getTensorShapeElementCount func(info uintptr, out *uintptr) uintptr
	getTensorTypeAndShape      func(value uintptr, out *uintptr) uintptr
	allocatorFree              func(allocator, p uintptr) uintptr
	getAllocatorWithDefault    func(out *uintptr) uintptr
	releaseStatus              func(status uintptr)
	releaseSession             func(session uintptr)
	releaseValue               func(value uintptr)
	releaseTypeInfo            func(info uintptr)
	releaseTensorTypeAndShape  func(info uintptr)
	releaseSessionOptions      func(options uintptr)
	addSessionConfigEntry      func(options uintptr, key, value string) uintptr
	getApi                     func(version uint32) uintptr
	getVersionString           func() string
}

// Runtime 已加载的 ONNX Runtime。一个进程只加载一次，之后可以建多个会话。
type Runtime struct {
	a         api
	env       uintptr
	allocator uintptr
	Version   string
	Path      string
}

var (
	loadMu  sync.Mutex
	loaded  *Runtime
	loadErr error
)

// ErrUnsupported 当前平台没有可用的 ONNX Runtime 绑定。
var ErrUnsupported = errors.New("onnxruntime is not supported on this platform")

// Load 加载指定路径的 onnxruntime 动态库。进程内只加载第一次成功的那一个。
func Load(libPath string) (*Runtime, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if loaded != nil {
		return loaded, nil
	}
	r, err := load(libPath)
	if err != nil {
		loadErr = err
		return nil, err
	}
	loaded, loadErr = r, nil
	return r, nil
}

// ptrAt 把 C 侧返回的地址转回指针，不经过 uintptr→unsafe.Pointer 的直接转换。
func ptrAt(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

func load(libPath string) (*Runtime, error) {
	handle, err := openLibrary(libPath)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", libPath, err)
	}
	baseFn, err := lookup(handle, "OrtGetApiBase")
	if err != nil {
		return nil, fmt.Errorf("%s is not onnxruntime: %w", libPath, err)
	}
	var getBase func() uintptr
	purego.RegisterFunc(&getBase, baseFn)
	base := getBase()
	if base == 0 {
		return nil, errors.New("OrtGetApiBase returned null")
	}
	r := &Runtime{Path: libPath}
	baseTable := unsafe.Slice((*uintptr)(ptrAt(base)), 2)
	purego.RegisterFunc(&r.a.getApi, baseTable[0])
	purego.RegisterFunc(&r.a.getVersionString, baseTable[1])
	r.Version = r.a.getVersionString()
	table := r.a.getApi(apiVersion)
	if table == 0 {
		return nil, fmt.Errorf("onnxruntime %s does not offer C API version %d", r.Version, apiVersion)
	}
	fns := unsafe.Slice((*uintptr)(ptrAt(table)), idxAddSessionConfigEntry+1)
	reg := func(fptr any, idx int) { purego.RegisterFunc(fptr, fns[idx]) }
	reg(&r.a.getErrorMessage, idxGetErrorMessage)
	reg(&r.a.createEnv, idxCreateEnv)
	reg(&r.a.createSession, idxCreateSession)
	reg(&r.a.run, idxRun)
	reg(&r.a.createSessionOptions, idxCreateSessionOptions)
	reg(&r.a.setSessionExecutionMode, idxSetSessionExecutionMode)
	reg(&r.a.disableMemPattern, idxDisableMemPattern)
	reg(&r.a.disableCpuMemArena, idxDisableCpuMemArena)
	reg(&r.a.setGraphOptimization, idxSetSessionGraphOptimization)
	reg(&r.a.setIntraOpNumThreads, idxSetIntraOpNumThreads)
	reg(&r.a.setInterOpNumThreads, idxSetInterOpNumThreads)
	reg(&r.a.sessionGetInputCount, idxSessionGetInputCount)
	reg(&r.a.sessionGetInputTypeInfo, idxSessionGetInputTypeInfo)
	reg(&r.a.sessionGetInputName, idxSessionGetInputName)
	reg(&r.a.sessionGetOutputName, idxSessionGetOutputName)
	reg(&r.a.createTensorAsOrtValue, idxCreateTensorAsOrtValue)
	reg(&r.a.getTensorMutableData, idxGetTensorMutableData)
	reg(&r.a.castTypeInfoToTensorInfo, idxCastTypeInfoToTensorInfo)
	reg(&r.a.getDimensionsCount, idxGetDimensionsCount)
	reg(&r.a.getDimensions, idxGetDimensions)
	reg(&r.a.getTensorShapeElementCount, idxGetTensorShapeElementCount)
	reg(&r.a.getTensorTypeAndShape, idxGetTensorTypeAndShape)
	reg(&r.a.allocatorFree, idxAllocatorFree)
	reg(&r.a.getAllocatorWithDefault, idxGetAllocatorWithDefaultOptions)
	reg(&r.a.releaseStatus, idxReleaseStatus)
	reg(&r.a.releaseSession, idxReleaseSession)
	reg(&r.a.releaseValue, idxReleaseValue)
	reg(&r.a.releaseTypeInfo, idxReleaseTypeInfo)
	reg(&r.a.releaseTensorTypeAndShape, idxReleaseTensorTypeAndShapeInfo)
	reg(&r.a.releaseSessionOptions, idxReleaseSessionOptions)
	reg(&r.a.addSessionConfigEntry, idxAddSessionConfigEntry)

	if err := r.check(r.a.createEnv(loggingLevelWarning, "musicfox\x00", &r.env)); err != nil {
		return nil, fmt.Errorf("create env: %w", err)
	}
	if err := r.check(r.a.getAllocatorWithDefault(&r.allocator)); err != nil {
		return nil, fmt.Errorf("default allocator: %w", err)
	}
	return r, nil
}

func (r *Runtime) check(status uintptr) error {
	if status == 0 {
		return nil
	}
	msg := r.a.getErrorMessage(status)
	r.a.releaseStatus(status)
	return errors.New(msg)
}

// SessionOptions 会话参数。
type SessionOptions struct {
	// Threads intra-op 线程数，<=0 用 ORT 默认（每个逻辑核一个，实测反而更慢）。
	Threads int
	// DisableCPUArena 关闭 CPU 内存竞技场。htdemucs 上单这一项就值约 5GB 峰值。
	DisableCPUArena bool
	// DisableMemPattern 关闭内存模式预分配。
	DisableMemPattern bool
}

// Session 一个已加载的模型。Run 不可并发调用。
type Session struct {
	r       *Runtime
	handle  uintptr
	mu      sync.Mutex
	inputs  []string
	outputs []string
}

// NewSession 从文件加载模型。
func (r *Runtime) NewSession(modelPath string, o SessionOptions) (*Session, error) {
	var opts uintptr
	if err := r.check(r.a.createSessionOptions(&opts)); err != nil {
		return nil, err
	}
	defer r.a.releaseSessionOptions(opts)
	steps := []func() uintptr{
		func() uintptr { return r.a.setSessionExecutionMode(opts, executionSequential) },
		func() uintptr { return r.a.setGraphOptimization(opts, graphOptimizeAll) },
		func() uintptr { return r.a.setInterOpNumThreads(opts, 1) },
		// 线程等活时不自旋：分析在后台跑，"看不见"比快重要。
		func() uintptr { return r.a.addSessionConfigEntry(opts, "session.intra_op.allow_spinning\x00", "0\x00") },
	}
	if o.Threads > 0 {
		steps = append(steps, func() uintptr { return r.a.setIntraOpNumThreads(opts, int32(o.Threads)) })
	}
	if o.DisableCPUArena {
		steps = append(steps, func() uintptr { return r.a.disableCpuMemArena(opts) })
	}
	if o.DisableMemPattern {
		steps = append(steps, func() uintptr { return r.a.disableMemPattern(opts) })
	}
	for _, step := range steps {
		if err := r.check(step()); err != nil {
			return nil, fmt.Errorf("session options: %w", err)
		}
	}
	path := toPathChars(modelPath)
	s := &Session{r: r}
	err := r.check(r.a.createSession(r.env, &path[0], opts, &s.handle))
	runtime.KeepAlive(path)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", modelPath, err)
	}
	if s.inputs, err = s.names(r.a.sessionGetInputName); err == nil {
		s.outputs, err = s.names(r.a.sessionGetOutputName)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) names(get func(session, index, allocator uintptr, out *uintptr) uintptr) ([]string, error) {
	r := s.r
	var names []string
	for i := uintptr(0); ; i++ {
		var p uintptr
		status := get(s.handle, i, r.allocator, &p)
		if status != 0 {
			// 越界即列举完毕。
			r.a.releaseStatus(status)
			break
		}
		names = append(names, cString(p))
		_ = r.a.allocatorFree(r.allocator, p)
		if i > 64 {
			break
		}
	}
	if len(names) == 0 {
		return nil, errors.New("model has no inputs or outputs")
	}
	return names, nil
}

func cString(p uintptr) string {
	if p == 0 {
		return ""
	}
	n := 0
	for *(*byte)(ptrAt(p + uintptr(n))) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(ptrAt(p)), n))
}

// Inputs 模型的输入名。
func (s *Session) Inputs() []string { return s.inputs }

// Outputs 模型的输出名。
func (s *Session) Outputs() []string { return s.outputs }

// InputShape 第 i 个输入的形状，动态维为 -1。
func (s *Session) InputShape(i int) ([]int64, error) {
	r := s.r
	var typeInfo, info uintptr
	if err := r.check(r.a.sessionGetInputTypeInfo(s.handle, uintptr(i), &typeInfo)); err != nil {
		return nil, err
	}
	defer r.a.releaseTypeInfo(typeInfo)
	// 转换出来的指针归 typeInfo 所有，不单独释放。
	if err := r.check(r.a.castTypeInfoToTensorInfo(typeInfo, &info)); err != nil {
		return nil, err
	}
	return r.dims(info)
}

func (r *Runtime) dims(info uintptr) ([]int64, error) {
	var n uintptr
	if err := r.check(r.a.getDimensionsCount(info, &n)); err != nil {
		return nil, err
	}
	if n == 0 {
		return []int64{}, nil
	}
	dims := make([]int64, n)
	if err := r.check(r.a.getDimensions(info, &dims[0], n)); err != nil {
		return nil, err
	}
	return dims, nil
}

// Tensor float32 张量。
type Tensor struct {
	Shape []int64
	Data  []float32
}

func elements(shape []int64) int {
	n := 1
	for _, d := range shape {
		n *= int(d)
	}
	return n
}

// Run 执行一次推理。输入按模型输入顺序，返回全部输出（按模型输出顺序）。
//
// 会阻塞调用它的 OS 线程直到推理结束：调用方必须在后台 goroutine 里调用。
func (s *Session) Run(inputs ...Tensor) ([]Tensor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.r
	if len(inputs) != len(s.inputs) {
		return nil, fmt.Errorf("model takes %d inputs, got %d", len(s.inputs), len(inputs))
	}
	values := make([]uintptr, len(inputs))
	defer func() {
		for _, v := range values {
			if v != 0 {
				r.a.releaseValue(v)
			}
		}
	}()
	for i, in := range inputs {
		if len(in.Shape) == 0 || elements(in.Shape) != len(in.Data) {
			return nil, fmt.Errorf("input %d: shape %v does not hold %d values", i, in.Shape, len(in.Data))
		}
		// 张量内存由 ORT 分配，再把数据拷进去：C 侧不持有任何 Go 指针。
		if err := r.check(r.a.createTensorAsOrtValue(r.allocator, &in.Shape[0], uintptr(len(in.Shape)), tensorFloat, &values[i])); err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		var p uintptr
		if err := r.check(r.a.getTensorMutableData(values[i], &p)); err != nil {
			return nil, err
		}
		copy(unsafe.Slice((*float32)(ptrAt(p)), len(in.Data)), in.Data)
	}

	inNames, keepIn := cStrings(s.inputs)
	outNames, keepOut := cStrings(s.outputs)
	outputs := make([]uintptr, len(s.outputs))
	err := r.check(r.a.run(s.handle, 0, &inNames[0], &values[0], uintptr(len(values)),
		&outNames[0], uintptr(len(outNames)), &outputs[0]))
	runtime.KeepAlive(keepIn)
	runtime.KeepAlive(keepOut)
	runtime.KeepAlive(inNames)
	runtime.KeepAlive(outNames)
	defer func() {
		for _, v := range outputs {
			if v != 0 {
				r.a.releaseValue(v)
			}
		}
	}()
	if err != nil {
		return nil, err
	}

	result := make([]Tensor, len(outputs))
	for i, v := range outputs {
		var info, count, p uintptr
		if err := r.check(r.a.getTensorTypeAndShape(v, &info)); err != nil {
			return nil, err
		}
		shape, err := r.dims(info)
		if err == nil {
			err = r.check(r.a.getTensorShapeElementCount(info, &count))
		}
		r.a.releaseTensorTypeAndShape(info)
		if err != nil {
			return nil, err
		}
		if err := r.check(r.a.getTensorMutableData(v, &p)); err != nil {
			return nil, err
		}
		data := make([]float32, count)
		if count > 0 {
			copy(data, unsafe.Slice((*float32)(ptrAt(p)), count))
		}
		result[i] = Tensor{Shape: shape, Data: data}
	}
	return result, nil
}

// cStrings 以 NUL 结尾的字节串及其首字节指针数组。两个返回值都须在调用期间保活。
func cStrings(names []string) ([]*byte, [][]byte) {
	bufs := make([][]byte, len(names))
	ptrs := make([]*byte, len(names))
	for i, n := range names {
		bufs[i] = append([]byte(n), 0)
		ptrs[i] = &bufs[i][0]
	}
	return ptrs, bufs
}

// Close 释放会话。
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle != 0 {
		s.r.a.releaseSession(s.handle)
		s.handle = 0
	}
}
