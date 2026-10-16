package gateway

import (
	"strings"
)

// Replace the complete held suffix with an ordinary assistant confirmation.
// No executable tool name, argument, start event or completed snapshot survives.
func rewriteGuardFrames(kind string, frames []toolGuardFrame, notice string, prefix map[int]any, sequence int) ([]byte, error) {
	var first map[string]any
	for _, frame := range frames {
		if frame.obj != nil {
			first = frame.obj
			break
		}
	}
	if first == nil {
		return nil, errUnknownToolProtocol
	}
	if _, ok := first["choices"]; ok {
		if kind == "json" {
			obj := cloneProtocolFrame(first)
			for _, raw := range obj["choices"].([]any) {
				choice, _ := raw.(map[string]any)
				delete(choice, "delta")
				choice["message"] = map[string]any{"role": "assistant", "content": notice}
				choice["finish_reason"] = "stop"
			}
			return encodeGuardFrame(kind, "", obj), nil
		}
		base := map[string]any{}
		for _, key := range []string{"id", "object", "created", "model", "system_fingerprint"} {
			if v, ok := first[key]; ok {
				base[key] = v
			}
		}
		indexes := map[string]any{}
		for _, frame := range frames {
			choices, _ := frame.obj["choices"].([]any)
			for i, raw := range choices {
				choice, _ := raw.(map[string]any)
				index := choice["index"]
				if index == nil {
					index = i
				}
				indexes[protocolIndex(index, i)] = index
			}
		}
		var choices []any
		for _, index := range indexes {
			choices = append(choices, map[string]any{"index": index, "delta": map[string]any{"role": "assistant", "content": notice}, "finish_reason": nil})
		}
		base["choices"] = choices
		out := encodeGuardFrame(kind, "", base)
		for _, raw := range choices {
			choice := raw.(map[string]any)
			choice["delta"] = map[string]any{}
			choice["finish_reason"] = "stop"
		}
		out = append(out, encodeGuardFrame(kind, "", base)...)
		for _, frame := range frames {
			if usage, ok := frame.obj["usage"]; ok {
				base["choices"] = []any{}
				base["usage"] = usage
				out = append(out, encodeGuardFrame(kind, "", base)...)
				break
			}
		}
		if kind == "sse" {
			out = append(out, []byte("data: [DONE]\n\n")...)
		}
		return out, nil
	}
	typ, _ := first["type"].(string)
	if typ == "content_block_start" || typ == "content_block_delta" || first["type"] == "message" {
		if kind == "json" {
			obj := cloneProtocolFrame(first)
			obj["content"] = []any{map[string]any{"type": "text", "text": notice}}
			obj["stop_reason"] = "end_turn"
			return encodeGuardFrame(kind, "", obj), nil
		}
		index := first["index"]
		if index == nil {
			index = 0
		}
		events := []map[string]any{
			{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}},
			{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": notice}},
			{"type": "content_block_stop", "index": index},
			{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}},
			{"type": "message_stop"},
		}
		var out []byte
		for _, obj := range events {
			out = append(out, encodeGuardFrame(kind, "event: "+obj["type"].(string)+"\n", obj)...)
		}
		return out, nil
	}
	if strings.HasPrefix(typ, "response.") || first["object"] == "response" || first["output"] != nil {
		item := map[string]any{"id": "msg_silencer_" + newRequestID(), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": notice, "annotations": []any{}}}}
		if kind == "json" {
			obj := cloneProtocolFrame(first)
			obj["output"] = []any{item}
			obj["status"] = "completed"
			return encodeGuardFrame(kind, "", obj), nil
		}
		index := first["output_index"]
		if index == nil {
			index = 0
		}
		response := map[string]any{"object": "response", "status": "completed"}
		seq := sequence
		if seq < 0 {
			if n, ok := numericMetaInt(first["sequence_number"]); ok {
				seq = n - 1
			}
		}
		for _, frame := range frames {
			if r, ok := frame.obj["response"].(map[string]any); ok {
				response = cloneProtocolFrame(r)
			}
		}
		idx, ok := numericMetaInt(index)
		if !ok {
			idx = 0
		}
		if idx < 0 || idx >= 128 {
			return nil, errUnknownToolProtocol
		}
		original, _ := response["output"].([]any)
		output := make([]any, 0, idx+1)
		for i := 0; i < idx; i++ {
			prior := prefix[i]
			if prior == nil && i < len(original) {
				prior = original[i]
			}
			if prior == nil {
				return nil, errUnknownToolProtocol
			}
			output = append(output, prior)
		}
		response["output"] = append(output, item)
		response["status"] = "completed"
		delete(response, "error")
		delete(response, "incomplete_details")
		events := []map[string]any{
			{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"id": item["id"], "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
			{"type": "response.content_part.added", "output_index": index, "item_id": item["id"], "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
			{"type": "response.output_text.delta", "output_index": index, "item_id": item["id"], "content_index": 0, "delta": notice},
			{"type": "response.output_text.done", "output_index": index, "item_id": item["id"], "content_index": 0, "text": notice},
			{"type": "response.content_part.done", "output_index": index, "item_id": item["id"], "content_index": 0, "part": map[string]any{"type": "output_text", "text": notice, "annotations": []any{}}},
			{"type": "response.output_item.done", "output_index": index, "item": item},
			{"type": "response.completed", "response": response},
		}
		var out []byte

		for _, obj := range events {
			seq++
			obj["sequence_number"] = seq
			out = append(out, encodeGuardFrame(kind, "event: "+obj["type"].(string)+"\n", obj)...)
		}
		return out, nil
	}
	if _, ok := first["message"].(map[string]any); ok {
		obj := cloneProtocolFrame(first)
		obj["message"] = map[string]any{"role": "assistant", "content": notice}
		obj["done"] = true
		obj["done_reason"] = "stop"
		return encodeGuardFrame(kind, "", obj), nil
	}
	if _, ok := first["candidates"].([]any); ok {
		obj := cloneProtocolFrame(first)
		for _, raw := range obj["candidates"].([]any) {
			candidate, _ := raw.(map[string]any)
			candidate["content"] = map[string]any{"role": "model", "parts": []any{map[string]any{"text": notice}}}
			candidate["finishReason"] = "STOP"
		}
		return encodeGuardFrame(kind, "", obj), nil
	}
	return nil, errUnknownToolProtocol
}
