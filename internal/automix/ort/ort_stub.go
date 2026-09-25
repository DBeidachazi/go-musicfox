//go:build !((linux || darwin || windows) && (amd64 || arm64))

package ort

import "errors"

// ErrUnsupported 当前平台没有可用的 ONNX Runtime 绑定。
var ErrUnsupported = errors.New("onnxruntime is not supported on this platform")

// Runtime 占位。
type Runtime struct {
	Version string
	Path    string
}

// SessionOptions 占位。
type SessionOptions struct {
	Threads           int
	DisableCPUArena   bool
	DisableMemPattern bool
}

// Session 占位。
type Session struct{}

// Tensor float32 张量。
type Tensor struct {
	Shape []int64
	Data  []float32
}

// Load 在不支持的平台上总是失败。
func Load(string) (*Runtime, error) { return nil, ErrUnsupported }

// NewSession 占位。
func (r *Runtime) NewSession(string, SessionOptions) (*Session, error) { return nil, ErrUnsupported }

// Inputs 占位。
func (s *Session) Inputs() []string { return nil }

// Outputs 占位。
func (s *Session) Outputs() []string { return nil }

// InputShape 占位。
func (s *Session) InputShape(int) ([]int64, error) { return nil, ErrUnsupported }

// Run 占位。
func (s *Session) Run(...Tensor) ([]Tensor, error) { return nil, ErrUnsupported }

// Close 占位。
func (s *Session) Close() {}
