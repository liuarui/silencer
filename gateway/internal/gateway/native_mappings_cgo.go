//go:build silencer_engine && cgo

package gateway

/*
#include <stdlib.h>
#include <stdint.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"runtime/cgo"
	"unsafe"
)

type nativeMappingAuthority struct {
	ctx   context.Context
	store DurableMappingStore
}

// The C++ shared request map owns the handle until every masking and restore
// stream is released. No Go pointer is stored across the ABI boundary.
//
//export silencerReleaseMapping
func silencerReleaseMapping(handle C.uintptr_t) { cgo.Handle(handle).Delete() }

//export silencerResolveMapping
func silencerResolveMapping(handle C.uintptr_t, input *C.char, size C.size_t) (result *C.char) {
	defer func() {
		if recover() != nil {
			result = C.CString(`{"ok":false}`)
		}
	}()
	state := cgo.Handle(handle).Value().(*nativeMappingAuthority)
	var request struct {
		Issue bool   `json:"issue"`
		Scope string `json:"scope"`
		Value string `json:"value"`
		Label string `json:"label"`
	}
	if size > 1<<30 || json.Unmarshal(C.GoBytes(unsafe.Pointer(input), C.int(size)), &request) != nil {
		return C.CString(`{"ok":false}`)
	}
	var reply any
	if request.Issue {
		mapping, err := state.store.IssueEntityMapping(state.ctx, request.Scope, request.Value, request.Label)
		if err != nil {
			return C.CString(`{"ok":false}`)
		}
		reply = map[string]any{"ok": true, "found": true, "token": mapping.Token, "original": mapping.Original}
	} else {
		mapping, found, err := state.store.ResolveEntityMapping(state.ctx, request.Scope, request.Value)
		if err != nil {
			return C.CString(`{"ok":false}`)
		}
		reply = map[string]any{"ok": true, "found": found, "token": mapping.Token, "original": mapping.Original}
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		return C.CString(`{"ok":false}`)
	}
	return C.CString(string(encoded))
}
