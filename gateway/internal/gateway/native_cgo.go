//go:build silencer_engine && cgo

package gateway

/*
#cgo CXXFLAGS: -std=c++17
#cgo CFLAGS: -I../../../engine/cpp
#cgo linux LDFLAGS: -lstdc++ -lsilencer_engine
#cgo darwin LDFLAGS: -lc++ -lsilencer_engine
#include <stdlib.h>
#include "engine.h"
extern char* silencerResolveMapping(uintptr_t, char*, size_t);
extern void silencerReleaseMapping(uintptr_t);
static char* mapping_bridge(uintptr_t handle, const char* json, size_t len) {
  return silencerResolveMapping(handle, (char*)json, len);
}
static int bind_mapping_resolver(engine_t* e, uint64_t id, uintptr_t handle) {
  return mask_stream_bind_mapping_resolver(e, id, handle, mapping_bridge, silencerReleaseMapping);
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/cgo"
	"sync"
	"unsafe"
)

type nativeEngine struct {
	mu  sync.RWMutex
	ptr *C.engine_t
}

// OffsetMap translates byte offsets in a masked payload back to source
// offsets. It owns the native map until Close is called. The map is optional
// at the Engine interface level because JSON field-path rewrites currently
// require parser spans and return an explicit native error.
type OffsetMap struct {
	mu  sync.Mutex
	ptr *C.offset_map_t
}

func (m *OffsetMap) Lookup(maskedOffset int) (int, error) {
	if m == nil || maskedOffset < 0 {
		return 0, fmt.Errorf("invalid offset map or offset")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ptr == nil {
		return 0, fmt.Errorf("offset map is closed")
	}
	var original C.size_t
	if rc := C.offset_map_lookup(m.ptr, C.size_t(maskedOffset), &original); rc != 0 {
		return 0, fmt.Errorf("offset_map_lookup: %s", C.GoString(C.last_error()))
	}
	if uint64(original) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("offset map result is too large")
	}
	return int(original), nil
}

func (m *OffsetMap) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ptr != nil {
		C.offset_map_free(m.ptr)
		m.ptr = nil
	}
}

// MaskWithMap performs ordinary text masking and returns an offset map owned
// by the caller. Call OffsetMap.Close after the map is no longer needed.
func (e *nativeEngine) MaskWithMap(ctx context.Context, body []byte) ([]byte, *OffsetMap, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ptr == nil {
		return nil, nil, fmt.Errorf("engine is closed")
	}
	in := C.CBytes(body)
	defer C.free(in)
	var out *C.char
	var outLen C.size_t
	var nativeMap *C.offset_map_t
	if rc := C.mask_request_with_map(e.ptr, (*C.char)(in), C.size_t(len(body)), &out, &outLen, &nativeMap); rc != 0 {
		if out != nil {
			C.buffer_free(unsafe.Pointer(out))
		}
		if nativeMap != nil {
			C.offset_map_free(nativeMap)
		}
		return nil, nil, fmt.Errorf("mask_request_with_map: %s", C.GoString(C.last_error()))
	}
	if uint64(outLen) > uint64(1<<31-1) {
		C.buffer_free(unsafe.Pointer(out))
		C.offset_map_free(nativeMap)
		return nil, nil, fmt.Errorf("mask output too large")
	}
	masked := C.GoBytes(unsafe.Pointer(out), C.int(outLen))
	C.buffer_free(unsafe.Pointer(out))
	return masked, &OffsetMap{ptr: nativeMap}, nil
}

func newNativeEngine(cfg Config) (Engine, error) {
	if version := uint32(C.engine_abi_version()); version != 4 {
		return nil, fmt.Errorf("unsupported C++ engine ABI version %d", version)
	}
	const requiredCapabilities = uint64(1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7)
	if capabilities := uint64(C.engine_capabilities()); capabilities&requiredCapabilities != requiredCapabilities {
		return nil, fmt.Errorf("C++ engine capabilities 0x%x missing required bits 0x%x", capabilities, requiredCapabilities)
	}
	var raw *C.char
	var n C.size_t
	if cfg.EngineConfig != "" {
		raw = C.CString(cfg.EngineConfig)
		defer C.free(unsafe.Pointer(raw))
		n = C.size_t(len(cfg.EngineConfig))
	}
	e := C.engine_create(raw, n)
	if e == nil {
		return nil, fmt.Errorf("engine_create: %s", C.GoString(C.last_error()))
	}
	return &nativeEngine{ptr: e}, nil
}

func (e *nativeEngine) Mask(ctx context.Context, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return []byte{}, nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ptr == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	in := C.CBytes(body)
	defer C.free(in)
	var out *C.char
	var outLen C.size_t
	rc := C.mask_request(e.ptr, (*C.char)(in), C.size_t(len(body)), &out, &outLen)
	if rc != 0 {
		return nil, fmt.Errorf("mask_request: %s", C.GoString(C.last_error()))
	}
	defer C.buffer_free(unsafe.Pointer(out))
	if uint64(outLen) > uint64(1<<31-1) {
		return nil, fmt.Errorf("mask_request output too large")
	}
	return C.GoBytes(unsafe.Pointer(out), C.int(outLen)), nil
}

func (e *nativeEngine) NewMaskingSession(context.Context) (MaskingSession, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ptr == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	id := C.mask_stream_begin_scoped(e.ptr)
	if id == 0 {
		return nil, fmt.Errorf("mask_stream_begin: %s", C.GoString(C.last_error()))
	}
	return &nativeMaskingSession{engine: e, id: id, scoped: true}, nil
}

type nativeMaskingSession struct {
	mu       sync.Mutex
	engine   *nativeEngine
	id       C.uint64_t
	scoped   bool
	finished bool
}

func (e *nativeEngine) NewReusableMaskingSession(ctx context.Context, scope string) (MaskingSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ptr == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	value := C.CString(scope)
	defer C.free(unsafe.Pointer(value))
	id := C.mask_stream_begin_reusable(e.ptr, value, C.size_t(len(scope)))
	if id == 0 {
		return nil, fmt.Errorf("mask_stream_begin_reusable: %s", C.GoString(C.last_error()))
	}
	return &nativeMaskingSession{engine: e, id: id, scoped: true}, nil
}

func (e *nativeEngine) NewDurableMaskingSession(ctx context.Context, scope string, store DurableMappingStore) (ContextualMaskingSession, error) {
	if store == nil {
		return nil, fmt.Errorf("durable mapping store unavailable")
	}
	session, err := e.NewReusableMaskingSession(ctx, scope)
	if err != nil {
		return nil, err
	}
	s := session.(*nativeMaskingSession)
	handle := cgo.NewHandle(&nativeMappingAuthority{ctx: ctx, store: store})
	e.mu.RLock()
	rc := C.bind_mapping_resolver(e.ptr, s.id, C.uintptr_t(handle))
	e.mu.RUnlock()
	if rc != 0 {
		handle.Delete()
		s.Release()
		return nil, fmt.Errorf("durable mapping binding unavailable")
	}
	return s, nil
}

func (e *nativeEngine) ImportReusableMappings(ctx context.Context, scope string, items []map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(items)
	if err != nil {
		return err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ptr == nil {
		return fmt.Errorf("engine is closed")
	}
	value := C.CString(scope)
	defer C.free(unsafe.Pointer(value))
	input := C.CBytes(body)
	defer C.free(input)
	if C.engine_import_reusable_tokens(e.ptr, value, C.size_t(len(scope)), (*C.char)(input), C.size_t(len(body))) != 0 {
		return fmt.Errorf("import reusable mappings: %s", C.GoString(C.last_error()))
	}
	return nil
}

func (e *nativeEngine) ClearReusableMappings() error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ptr == nil {
		return fmt.Errorf("engine is closed")
	}
	if C.engine_clear_reusable_tokens(e.ptr) != 0 {
		return fmt.Errorf("clear reusable mappings: %s", C.GoString(C.last_error()))
	}
	return nil
}

func (s *nativeMaskingSession) Push(ctx context.Context, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.call(body, false)
}

func (s *nativeMaskingSession) Finish(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.call(nil, true)
}

func (s *nativeMaskingSession) MaskedItems() ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == 0 || !s.finished || !s.scoped || s.engine == nil {
		return nil, fmt.Errorf("mask stream items are unavailable")
	}
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	if s.engine.ptr == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	var out *C.char
	var outLen C.size_t
	if rc := C.mask_stream_items_json(s.engine.ptr, s.id, &out, &outLen); rc != 0 {
		return nil, fmt.Errorf("mask_stream_items_json: %s", C.GoString(C.last_error()))
	}
	defer C.buffer_free(unsafe.Pointer(out))
	if uint64(outLen) > uint64(1<<31-1) {
		return nil, fmt.Errorf("mask stream items too large")
	}
	var items []map[string]any
	if err := json.Unmarshal(C.GoBytes(unsafe.Pointer(out), C.int(outLen)), &items); err != nil {
		return nil, fmt.Errorf("decode mask stream items: %w", err)
	}
	return items, nil
}

func (s *nativeMaskingSession) Abort() {
	if s == nil || s.engine == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == 0 {
		return
	}
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	if s.engine.ptr == nil {
		s.id = 0
		return
	}
	if s.finished {
		_ = C.mask_stream_release(s.engine.ptr, s.id)
	} else {
		_ = C.mask_stream_abort(s.engine.ptr, s.id)
	}
	s.id = 0
	s.finished = true
}

// Seal retains the mapping for an upstream response that arrived before EOF.
// Pending input is discarded and is never emitted to the upstream.
func (s *nativeMaskingSession) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == 0 || s.engine == nil {
		return fmt.Errorf("mask stream is closed")
	}
	if s.finished {
		return nil
	}
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	if s.engine.ptr == nil {
		return fmt.Errorf("engine is closed")
	}
	if rc := C.mask_stream_seal(s.engine.ptr, s.id); rc != 0 {
		return fmt.Errorf("mask_stream_seal: %s", C.GoString(C.last_error()))
	}
	s.finished = true
	return nil
}

// Release drops a completed request-local mapping when the upstream response
// is binary or otherwise does not need placeholder restoration.
func (s *nativeMaskingSession) Release() {
	if s == nil || s.engine == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == 0 {
		return
	}
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	if s.engine.ptr != nil {
		if s.finished {
			_ = C.mask_stream_release(s.engine.ptr, s.id)
		} else {
			_ = C.mask_stream_abort(s.engine.ptr, s.id)
		}
	}
	s.id = 0
	s.finished = true
}

// NewRestoreSession binds an independent channel to this request's mapping.
// Keep the mask handle until Release so parallel choices and tool calls can
// create separate incremental streams without sharing their pending suffixes.
func (s *nativeMaskingSession) NewRestoreSession(ctx context.Context) (RestoreSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == 0 || !s.finished || !s.scoped {
		return nil, fmt.Errorf("mask stream is not ready for restoration")
	}
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	if s.engine.ptr == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	id := C.restore_stream_begin_for_mask(s.engine.ptr, s.id)
	if id == 0 {
		return nil, fmt.Errorf("restore_stream_begin_for_mask: %s", C.GoString(C.last_error()))
	}
	return &nativeRestoreSession{engine: s.engine, id: id}, nil
}

func (s *nativeMaskingSession) call(body []byte, final bool) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == 0 || s.engine == nil {
		return nil, fmt.Errorf("mask stream is closed")
	}
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	if s.engine.ptr == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	var out *C.char
	var outLen C.size_t
	var rc C.int
	if final {
		rc = C.mask_stream_finish(s.engine.ptr, s.id, &out, &outLen)
	} else {
		in := C.CBytes(body)
		defer C.free(in)
		rc = C.mask_stream_push(s.engine.ptr, s.id, (*C.char)(in), C.size_t(len(body)), &out, &outLen)
	}
	if rc != 0 {
		errText := C.GoString(C.last_error())
		_ = C.mask_stream_abort(s.engine.ptr, s.id)
		s.id = 0
		return nil, fmt.Errorf("mask stream: %s", errText)
	}
	if final {
		s.finished = true
		if !s.scoped {
			s.id = 0
		}
	}
	defer C.buffer_free(unsafe.Pointer(out))
	if uint64(outLen) > uint64(1<<31-1) {
		return nil, fmt.Errorf("mask output too large")
	}
	return C.GoBytes(unsafe.Pointer(out), C.int(outLen)), nil
}

type nativeRestoreSession struct {
	mu     sync.Mutex
	engine *nativeEngine
	id     C.uint64_t
}

func (e *nativeEngine) NewRestoreSession(context.Context) (RestoreSession, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ptr == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	id := C.restore_stream_begin(e.ptr)
	if id == 0 {
		return nil, fmt.Errorf("restore_stream_begin: %s", C.GoString(C.last_error()))
	}
	return &nativeRestoreSession{engine: e, id: id}, nil
}

func (s *nativeRestoreSession) Push(ctx context.Context, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.call(body, false)
}

func (s *nativeRestoreSession) Finish(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		s.Abort()
		return nil, err
	}
	return s.call(nil, true)
}

func (s *nativeRestoreSession) SetJSONEscape(ctx context.Context, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == 0 || s.engine == nil {
		return fmt.Errorf("restore stream is closed")
	}
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	if s.engine.ptr == nil {
		return fmt.Errorf("engine is closed")
	}
	var options C.uint32_t
	if enabled {
		options = C.SILENCER_RESTORE_OPTION_JSON_ESCAPE
	}
	if rc := C.restore_stream_set_options(s.engine.ptr, s.id, options); rc != 0 {
		return fmt.Errorf("restore_stream_set_options: %s", C.GoString(C.last_error()))
	}
	return nil
}

func (s *nativeRestoreSession) Abort() {
	if s == nil || s.engine == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == 0 {
		return
	}
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	if s.engine.ptr == nil {
		s.id = 0
		return
	}
	_ = C.restore_stream_abort(s.engine.ptr, s.id)
	s.id = 0
}

func (s *nativeRestoreSession) call(body []byte, final bool) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == 0 || s.engine == nil {
		return nil, fmt.Errorf("restore stream is closed")
	}
	s.engine.mu.RLock()
	defer s.engine.mu.RUnlock()
	if s.engine.ptr == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	var out *C.char
	var outLen C.size_t
	var rc C.int
	if final {
		rc = C.restore_stream_finish(s.engine.ptr, s.id, &out, &outLen)
	} else {
		in := C.CBytes(body)
		defer C.free(in)
		rc = C.restore_stream_push(s.engine.ptr, s.id, (*C.char)(in), C.size_t(len(body)), &out, &outLen)
	}
	if rc != 0 {
		errText := C.GoString(C.last_error())
		// A failed native call may have appended input before an allocation or
		// conversion error. Close the session so callers cannot retry against a
		// partially mutated stream.
		_ = C.restore_stream_abort(s.engine.ptr, s.id)
		s.id = 0
		return nil, fmt.Errorf("restore stream: %s", errText)
	}
	if final {
		s.id = 0
	}
	defer C.buffer_free(unsafe.Pointer(out))
	if uint64(outLen) > uint64(1<<31-1) {
		return nil, fmt.Errorf("restore output too large")
	}
	return C.GoBytes(unsafe.Pointer(out), C.int(outLen)), nil
}

func (e *nativeEngine) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr != nil {
		C.engine_destroy(e.ptr)
		e.ptr = nil
	}
}
