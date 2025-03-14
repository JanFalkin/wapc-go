package wazero

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/assemblyscript"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/wapc/wapc-go"
)

const i32 = api.ValueTypeI32

// functionStart is the name of the nullary function a module exports if it is a WASI Command Module.
//
// See https://github.com/WebAssembly/WASI/blob/snapshot-01/design/application-abi.md#current-unstable-abi
const functionStart = "_start"

// functionInit is the name of the nullary function that initializes waPC.
const functionInit = "wapc_init"

// functionGuestCall is a callback required to be exported. Below is its signature in WebAssembly 1.0 (MVP) Text Format:
//
//	(func $__guest_call (param $operation_len i32) (param $payload_len i32) (result (;errno;) i32))
const functionGuestCall = "__guest_call"

type (
	engine struct{ newRuntime NewRuntime }

	// Module represents a compiled waPC module.
	Module struct {
		// wapcHostCallHandler is the value of wapcHost.callHandler
		wapcHostCallHandler wapc.HostCallHandler

		runtime  wazero.Runtime
		compiled wazero.CompiledModule

		instanceCounter uint64

		config wazero.ModuleConfig

		// closed is atomically updated to ensure Close is only invoked once.
		closed uint32
	}

	Instance struct {
		name      string
		m         api.Module
		guestCall api.Function

		// closed is atomically updated to ensure Close is only invoked once.
		closed uint32
	}

	invokeContext struct {
		operation string

		guestReq  []byte
		guestResp []byte
		guestErr  string

		hostResp []byte
		hostErr  error
	}
)

// Ensure the engine conforms to the waPC interface.
var _ = (wapc.Module)((*Module)(nil))
var _ = (wapc.Instance)((*Instance)(nil))

var engineInstance = engine{newRuntime: DefaultRuntime}

// Engine returns a new wapc.Engine which uses the DefaultRuntime.
func Engine() wapc.Engine {
	return &engineInstance
}

// NewRuntime returns a new wazero runtime which is called when the New method
// on wapc.Engine is called. The result is closed upon wapc.Module Close.
type NewRuntime func(context.Context) (wazero.Runtime, error)

// EngineWithRuntime allows you to customize or return an alternative to
// DefaultRuntime,
func EngineWithRuntime(newRuntime NewRuntime) wapc.Engine {
	return &engine{newRuntime: newRuntime}
}

func (e *engine) Name() string {
	return "wazero"
}

// DefaultRuntime implements NewRuntime by returning a wazero runtime with WASI
// and AssemblyScript host functions instantiated.
func DefaultRuntime(ctx context.Context) (wazero.Runtime, error) {
	r := wazero.NewRuntime(ctx)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		_ = r.Close(ctx)
		return nil, err
	}

	// This disables the abort message as no other engines write it.
	envBuilder := r.NewHostModuleBuilder("env")
	assemblyscript.NewFunctionExporter().WithAbortMessageDisabled().ExportFunctions(envBuilder)
	if _, err := envBuilder.Instantiate(ctx); err != nil {
		_ = r.Close(ctx)
		return nil, err
	}
	return r, nil
}

// New implements the same method as documented on wapc.Engine.
func (e *engine) New(ctx context.Context, host wapc.HostCallHandler, guest []byte, config *wapc.ModuleConfig) (mod wapc.Module, err error) {
	r, err := e.newRuntime(ctx)
	if err != nil {
		return nil, err
	}

	m := &Module{runtime: r, wapcHostCallHandler: host}

	m.config = wazero.NewModuleConfig().
		WithStartFunctions(functionStart, functionInit) // Call any WASI or waPC start functions on instantiate.

	if config.Stdout != nil {
		m.config = m.config.WithStdout(config.Stdout)
	}
	if config.Stderr != nil {
		m.config = m.config.WithStderr(config.Stderr)
	}
	mod = m

	if _, err = instantiateWapcHost(ctx, r, m.wapcHostCallHandler, config.Logger); err != nil {
		_ = r.Close(ctx)
		return
	}

	if m.compiled, err = r.CompileModule(ctx, guest); err != nil {
		_ = r.Close(ctx)
		return
	}
	return
}

// UnwrapRuntime allows access to wazero-specific runtime features.
func (m *Module) UnwrapRuntime() *wazero.Runtime {
	return &m.runtime
}

// WithConfig allows you to override or replace wazero.ModuleConfig used to instantiate each guest.
// This can be used to configure clocks or filesystem access.
//
// The default (function input) conflgures WASI and waPC init functions as well as stdout and stderr.
func (m *Module) WithConfig(callback func(wazero.ModuleConfig) wazero.ModuleConfig) {
	m.config = callback(m.config)
}

// wapcHost implements all required waPC host function exports.
//
// See https://wapc.io/docs/spec/#required-host-exports
type wapcHost struct {
	// callHandler implements hostCall, which returns false (0) when nil.
	callHandler wapc.HostCallHandler

	// logger is used to implement consoleLog.
	logger wapc.Logger
}

// instantiateWapcHost instantiates a wapcHost and returns it and its corresponding module, or an error.
//   - r: used to instantiate the waPC host module
//   - callHandler: used to implement hostCall
//   - logger: used to implement consoleLog
func instantiateWapcHost(ctx context.Context, r wazero.Runtime, callHandler wapc.HostCallHandler, logger wapc.Logger) (api.Module, error) {
	h := &wapcHost{callHandler: callHandler, logger: logger}
	// Export host functions (in the order defined in https://wapc.io/docs/spec/#required-host-exports)
	// Note: These are defined manually (without reflection) for higher performance as waPC is a foundational library.
	return r.NewHostModuleBuilder("wapc").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(h.hostCall), []api.ValueType{i32, i32, i32, i32, i32, i32, i32, i32}, []api.ValueType{i32}).
		WithParameterNames("bind_ptr", "bind_len", "ns_ptr", "ns_len", "cmd_ptr", "cmd_len", "payload_ptr", "payload_len").
		Export("__host_call").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(h.consoleLog), []api.ValueType{i32, i32}, []api.ValueType{}).
		WithParameterNames("ptr", "len").
		Export("__console_log").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(h.guestRequest), []api.ValueType{i32, i32}, []api.ValueType{}).
		WithParameterNames("op_ptr", "ptr").
		Export("__guest_request").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(h.hostResponse), []api.ValueType{i32}, []api.ValueType{}).
		WithParameterNames("ptr").
		Export("__host_response").
		NewFunctionBuilder().
		WithGoFunction(api.GoFunc(h.hostResponseLen), []api.ValueType{}, []api.ValueType{i32}).
		Export("__host_response_len").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(h.guestResponse), []api.ValueType{i32, i32}, []api.ValueType{}).
		WithParameterNames("ptr", "len").
		Export("__guest_response").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(h.guestError), []api.ValueType{i32, i32}, []api.ValueType{}).
		WithParameterNames("ptr", "len").
		Export("__guest_error").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(h.hostError), []api.ValueType{i32}, []api.ValueType{}).
		WithParameterNames("ptr").
		Export("__host_error").
		NewFunctionBuilder().
		WithGoFunction(api.GoFunc(h.hostErrorLen), []api.ValueType{}, []api.ValueType{i32}).
		Export("__host_error_len").
		Instantiate(ctx)
}

// hostCall is the WebAssembly function export "__host_call", which initiates a host using the callHandler using
// parameters read from linear memory (wasm.Memory).
func (w *wapcHost) hostCall(ctx context.Context, m api.Module, stack []uint64) {
	bindPtr := uint32(stack[0])
	bindLen := uint32(stack[1])
	nsPtr := uint32(stack[2])
	nsLen := uint32(stack[3])
	cmdPtr := uint32(stack[4])
	cmdLen := uint32(stack[5])
	payloadPtr := uint32(stack[6])
	payloadLen := uint32(stack[7])

	ic := fromInvokeContext(ctx)
	if ic == nil || w.callHandler == nil {
		stack[0] = 0 // false: neither an invocation context, nor a callHandler
		return
	}

	mem := m.Memory()
	binding := requireReadString(mem, "binding", bindPtr, bindLen)
	namespace := requireReadString(mem, "namespace", nsPtr, nsLen)
	operation := requireReadString(mem, "operation", cmdPtr, cmdLen)
	payload := requireRead(mem, "payload", payloadPtr, payloadLen)

	if ic.hostResp, ic.hostErr = w.callHandler(ctx, binding, namespace, operation, payload); ic.hostErr != nil {
		stack[0] = 0 // false: error (assumed to be logged already?)
	} else {
		stack[0] = 1 // true
	}
}

// consoleLog is the WebAssembly function export "__console_log", which logs the message stored by the guest at the
// given offset (ptr) and length (len) in linear memory (wasm.Memory).
func (w *wapcHost) consoleLog(_ context.Context, m api.Module, params []uint64) {
	ptr := uint32(params[0])
	len := uint32(params[1])

	if log := w.logger; log != nil {
		msg := requireReadString(m.Memory(), "msg", ptr, len)
		w.logger(msg)
	}
}

// guestRequest is the WebAssembly function export "__guest_request", which writes the invokeContext.operation and
// invokeContext.guestReq to the given offsets (opPtr, ptr) in linear memory (wasm.Memory).
func (w *wapcHost) guestRequest(ctx context.Context, m api.Module, params []uint64) {
	opPtr := uint32(params[0])
	ptr := uint32(params[1])

	ic := fromInvokeContext(ctx)
	if ic == nil {
		return // no invoke context
	}

	mem := m.Memory()
	if operation := ic.operation; operation != "" {
		mem.Write(opPtr, []byte(operation))
	}
	if guestReq := ic.guestReq; guestReq != nil {
		mem.Write(ptr, guestReq)
	}
}

// hostResponse is the WebAssembly function export "__host_response", which writes the invokeContext.hostResp to the
// given offset (ptr) in linear memory (wasm.Memory).
func (w *wapcHost) hostResponse(ctx context.Context, m api.Module, params []uint64) {
	ptr := uint32(params[0])

	if ic := fromInvokeContext(ctx); ic == nil {
		return // no invoke context
	} else if hostResp := ic.hostResp; hostResp != nil {
		m.Memory().Write(ptr, hostResp)
	}
}

// hostResponse is the WebAssembly function export "__host_response_len", which returns the length of the current host
// response from invokeContext.hostResp.
func (w *wapcHost) hostResponseLen(ctx context.Context, results []uint64) {
	var hostResponseLen uint32
	if ic := fromInvokeContext(ctx); ic == nil {
		results[0] = 0 // no invoke context
	} else if hostResp := ic.hostResp; hostResp != nil {
		hostResponseLen = uint32(len(hostResp))
		results[0] = uint64(hostResponseLen)
	} else {
		results[0] = 0 // no host response
	}
}

// guestResponse is the WebAssembly function export "__guest_response", which reads invokeContext.guestResp from the
// given offset (ptr) and length (len) in linear memory (wasm.Memory).
func (w *wapcHost) guestResponse(ctx context.Context, m api.Module, params []uint64) {
	ptr := uint32(params[0])
	len := uint32(params[1])

	if ic := fromInvokeContext(ctx); ic == nil {
		return // no invoke context
	} else {
		ic.guestResp = requireRead(m.Memory(), "guestResp", ptr, len)
	}
}

// guestError is the WebAssembly function export "__guest_error", which reads invokeContext.guestErr from the given
// offset (ptr) and length (len) in linear memory (wasm.Memory).
func (w *wapcHost) guestError(ctx context.Context, m api.Module, params []uint64) {
	ptr := uint32(params[0])
	len := uint32(params[1])

	if ic := fromInvokeContext(ctx); ic == nil {
		return // no invoke context
	} else {
		ic.guestErr = requireReadString(m.Memory(), "guestErr", ptr, len)
	}
}

// hostError is the WebAssembly function export "__host_error", which writes the invokeContext.hostErr to the given
// offset (ptr) in linear memory (wasm.Memory).
func (w *wapcHost) hostError(ctx context.Context, m api.Module, params []uint64) {
	ptr := uint32(params[0])

	if ic := fromInvokeContext(ctx); ic == nil {
		return // no invoke context
	} else if hostErr := ic.hostErr; hostErr != nil {
		m.Memory().Write(ptr, []byte(hostErr.Error()))
	}
}

// hostError is the WebAssembly function export "__host_error_len", which returns the length of the current host error
// from invokeContext.hostErr.
func (w *wapcHost) hostErrorLen(ctx context.Context, results []uint64) {
	var hostErrorLen uint32
	if ic := fromInvokeContext(ctx); ic == nil {
		results[0] = 0 // no invoke context
	} else if hostErr := ic.hostErr; hostErr != nil {
		hostErrorLen = uint32(len(hostErr.Error()))
		results[0] = uint64(hostErrorLen)
	} else {
		results[0] = 0 // no host error
	}
}

// Instantiate implements the same method as documented on wapc.Module.
func (m *Module) Instantiate(ctx context.Context) (wapc.Instance, error) {
	if closed := atomic.LoadUint32(&m.closed); closed != 0 {
		return nil, errors.New("cannot Instantiate when a module is closed")
	}
	// Note: There's still a race below, even if the above check is still useful.

	moduleName := fmt.Sprintf("%d", atomic.AddUint64(&m.instanceCounter, 1))

	module, err := m.runtime.InstantiateModule(ctx, m.compiled, m.config.WithName(moduleName))
	if err != nil {
		return nil, err
	}

	instance := Instance{name: moduleName, m: module}

	if instance.guestCall = module.ExportedFunction(functionGuestCall); instance.guestCall == nil {
		_ = module.Close(ctx)
		return nil, fmt.Errorf("module %s didn't export function %s", moduleName, functionGuestCall)
	}

	return &instance, nil
}

// MemorySize implements the same method as documented on wapc.Instance.
func (i *Instance) MemorySize() uint32 {
	return i.m.Memory().Size()
}

type invokeContextKey struct{}

func newInvokeContext(ctx context.Context, ic *invokeContext) context.Context {
	return context.WithValue(ctx, invokeContextKey{}, ic)
}

// fromInvokeContext returns invokeContext value or nil if there was none.
//
// Note: This is never nil if called by Instance.Invoke
// TODO: It may be better to panic on nil or error as if this is nil, it is a bug in waPC, as no other path calls this.
func fromInvokeContext(ctx context.Context) *invokeContext {
	ic, _ := ctx.Value(invokeContextKey{}).(*invokeContext)
	return ic
}

// UnwrapModule allows access to wazero-specific api.Module.
func (i *Instance) UnwrapModule() api.Module {
	return i.m
}

// Invoke implements the same method as documented on wapc.Instance.
func (i *Instance) Invoke(ctx context.Context, operation string, payload []byte) ([]byte, error) {
	if closed := atomic.LoadUint32(&i.closed); closed != 0 {
		return nil, fmt.Errorf("error invoking guest with closed instance")
	}
	// Note: There's still a race below, even if the above check is still useful.

	ic := invokeContext{operation: operation, guestReq: payload}
	ctx = newInvokeContext(ctx, &ic)

	results, err := i.guestCall.Call(ctx, uint64(len(operation)), uint64(len(payload)))
	if err != nil {
		return nil, fmt.Errorf("error invoking guest: %w", err)
	}
	if ic.guestErr != "" { // guestErr is not nil if the guest called "__guest_error".
		return nil, errors.New(ic.guestErr)
	}

	result := results[0]
	success := result == 1

	if success { // guestResp is not nil if the guest called "__guest_response".
		return ic.guestResp, nil
	}

	return nil, fmt.Errorf("call to %q was unsuccessful", operation)
}

// Close implements the same method as documented on wapc.Instance.
func (i *Instance) Close(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&i.closed, 0, 1) {
		return nil
	}
	return i.m.Close(ctx)
}

// Close implements the same method as documented on wapc.Module.
func (m *Module) Close(ctx context.Context) (err error) {
	if !atomic.CompareAndSwapUint32(&m.closed, 0, 1) {
		return
	}
	err = m.runtime.Close(ctx) // closes everything
	m.runtime = nil
	return
}

// requireReadString is a convenience function that casts requireRead
func requireReadString(mem api.Memory, fieldName string, offset, byteCount uint32) string {
	return string(requireRead(mem, fieldName, offset, byteCount))
}

// requireRead is like api.Memory except that it panics if the offset and byteCount are out of range.
func requireRead(mem api.Memory, fieldName string, offset, byteCount uint32) []byte {
	buf, ok := mem.Read(offset, byteCount)
	if !ok {
		panic(fmt.Errorf("out of memory reading %s", fieldName))
	}
	return buf
}
