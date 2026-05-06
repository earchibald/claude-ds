// Tests for vision routing (CDS-19).
//
// Coverage matrix (from the issue acceptance criteria):
//   - empty cfg.VisionModel disables routing (Routed=false, body unchanged)
//   - body with no images is a passthrough (Routed=false, body unchanged)
//   - single image in last user turn → routed, model overridden, image
//     consolidated to the front of the last user content list
//   - image in earlier user turn → moved to last user turn (with
//     placeholder text replacing the original image block)
//   - image nested in tool_result → flattened into last user turn
//   - tool_use blocks → text labels with the exact Python-style format
//   - tools and tool_choice top-level keys dropped
//   - multi-image scenario across multiple turns (collected order:
//     traversal order, prepended to last user)
//   - non-JSON body passes through unchanged with no error
//
// Tests use `routeVision` directly so they cover the contract without
// booting the proxy. The end-to-end "vision skips effort" check is in
// rewrite_test.go (TestVisionSkipsEffortRewrite).
package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// decodeOrFail is a tiny helper so each test reads as a single
// declarative comparison rather than three lines of unmarshal noise.
func decodeOrFail(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	return obj
}

func TestRouteVision_DisabledWhenVisionModelEmpty(t *testing.T) {
	cfg := &Config{VisionModel: ""}
	in := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if info.Routed {
		t.Fatalf("expected Routed=false when VisionModel empty")
	}
	if !info.Disabled {
		t.Fatalf("expected Disabled=true when image present but VisionModel empty")
	}
	// Image counts must still be populated so SigNoz can see the
	// would-have-routed volume when routing is turned off.
	if info.ImageCount != 1 {
		t.Fatalf("ImageCount = %d, want 1 (must count even when disabled)", info.ImageCount)
	}
	if info.ImagesCollected != 1 {
		t.Fatalf("ImagesCollected = %d, want 1 (must mirror ImageCount when disabled)", info.ImagesCollected)
	}
	if string(out) != string(in) {
		t.Fatalf("body mutated when routing disabled:\nin:  %s\nout: %s", in, out)
	}
}

func TestRouteVision_DisabledWithNoImagesIsCleanPassthrough(t *testing.T) {
	cfg := &Config{VisionModel: ""}
	in := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// No image + disabled config = nothing to report at all.
	if info.Routed || info.Disabled || info.ImageCount != 0 {
		t.Fatalf("expected zero VisionInfo, got %+v", info)
	}
}

func TestRouteVision_DisabledCountsImageInToolResult(t *testing.T) {
	// Mirrors the routed-path's tool_result handling: even when
	// disabled, the count must include images nested inside
	// tool_result blocks so the disabled-path metric matches what the
	// routed-path would have collected.
	cfg := &Config{VisionModel: ""}
	in := []byte(`{
		"model": "deepseek-v4-pro",
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": [
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "BBBB"}}
				]}
			]}
		]
	}`)
	_, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !info.Disabled || info.ImageCount != 2 {
		t.Fatalf("Disabled=%v ImageCount=%d, want Disabled=true ImageCount=2",
			info.Disabled, info.ImageCount)
	}
}

func TestRouteVision_NoImagesIsPassthrough(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if info.Routed {
		t.Fatalf("expected Routed=false when body has no images")
	}
	if string(out) != string(in) {
		t.Fatalf("body mutated for non-image request")
	}
}

func TestRouteVision_NonJSONPassthrough(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`not json at all`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if info.Routed {
		t.Fatalf("expected Routed=false for non-JSON")
	}
	if string(out) != string(in) {
		t.Fatalf("body mutated for non-JSON request")
	}
}

func TestRouteVision_SingleImageInLastUser(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`{
		"model": "deepseek-v4-pro",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "what is in this image?"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
			]}
		]
	}`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !info.Routed {
		t.Fatalf("expected Routed=true")
	}
	if info.ModelTo != "deepseek-chat" || info.ModelFrom != "deepseek-v4-pro" {
		t.Fatalf("ModelFrom/To wrong: from=%q to=%q", info.ModelFrom, info.ModelTo)
	}
	if info.ImageCount != 1 {
		t.Fatalf("ImageCount = %d, want 1", info.ImageCount)
	}

	obj := decodeOrFail(t, out)
	if obj["model"] != "deepseek-chat" {
		t.Fatalf("model not overridden, got %v", obj["model"])
	}
	msgs := obj["messages"].([]any)
	last := msgs[0].(map[string]any)
	content := last["content"].([]any)
	// Image was prepended (Python: images_collected + last_content).
	first := content[0].(map[string]any)
	if first["type"] != "image" {
		t.Fatalf("expected image at front, got %v", first)
	}
}

func TestRouteVision_ImageInEarlierTurnMovesToLast(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`{
		"model": "deepseek-v4-pro",
		"messages": [
			{"role": "user", "content": [
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}},
				{"type": "text", "text": "first turn"}
			]},
			{"role": "assistant", "content": [{"type": "text", "text": "ok"}]},
			{"role": "user", "content": [{"type": "text", "text": "second turn"}]}
		]
	}`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !info.Routed || info.ImageCount != 1 {
		t.Fatalf("info wrong: %+v", info)
	}
	obj := decodeOrFail(t, out)
	msgs := obj["messages"].([]any)

	// Earlier user turn: image was replaced with placeholder text.
	first := msgs[0].(map[string]any)
	firstContent := first["content"].([]any)
	if len(firstContent) != 2 {
		t.Fatalf("first user content len = %d, want 2", len(firstContent))
	}
	first0 := firstContent[0].(map[string]any)
	if first0["type"] != "text" || first0["text"] != imagePlaceholder {
		t.Fatalf("first user [0] not placeholder: %+v", first0)
	}

	// Last user turn: image prepended, then original text.
	last := msgs[2].(map[string]any)
	lastContent := last["content"].([]any)
	if len(lastContent) != 2 {
		t.Fatalf("last user content len = %d, want 2", len(lastContent))
	}
	last0 := lastContent[0].(map[string]any)
	if last0["type"] != "image" {
		t.Fatalf("last user [0] not image: %+v", last0)
	}
	last1 := lastContent[1].(map[string]any)
	if last1["type"] != "text" || last1["text"] != "second turn" {
		t.Fatalf("last user [1] not original text: %+v", last1)
	}
}

func TestRouteVision_NestedToolResultImage(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`{
		"model": "deepseek-v4-pro",
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "x", "content": [
					{"type": "text", "text": "preamble"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
				]}
			]}
		]
	}`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !info.Routed || info.ImageCount != 1 {
		t.Fatalf("info wrong: %+v", info)
	}
	obj := decodeOrFail(t, out)
	msgs := obj["messages"].([]any)
	last := msgs[0].(map[string]any)
	content := last["content"].([]any)
	// Expect: image (extracted), then the text the tool_result wrapped.
	// tool_result wrapper itself is gone.
	if len(content) != 2 {
		t.Fatalf("content len = %d (%+v)", len(content), content)
	}
	if content[0].(map[string]any)["type"] != "image" {
		t.Fatalf("[0] not image: %+v", content[0])
	}
	if content[1].(map[string]any)["text"] != "preamble" {
		t.Fatalf("[1] not flattened text: %+v", content[1])
	}
}

func TestRouteVision_ToolUseBlockConverted(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`{
		"model": "deepseek-v4-pro",
		"messages": [
			{"role": "user", "content": [
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
			]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "u1", "name": "Read", "input": {"file_path": "/tmp/img.png"}}
			]},
			{"role": "user", "content": [{"type": "text", "text": "what is it"}]}
		]
	}`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if info.ToolUseConverted != 1 {
		t.Fatalf("ToolUseConverted = %d, want 1", info.ToolUseConverted)
	}
	obj := decodeOrFail(t, out)
	msgs := obj["messages"].([]any)
	asst := msgs[1].(map[string]any)
	asstContent := asst["content"].([]any)
	if len(asstContent) != 1 {
		t.Fatalf("assistant content len = %d", len(asstContent))
	}
	block := asstContent[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("expected text block: %+v", block)
	}
	want := "[used Read tool: file_path='/tmp/img.png']"
	if block["text"] != want {
		t.Fatalf("text = %q, want %q", block["text"], want)
	}
}

func TestRouteVision_ToolUseEmptyInput(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`{
		"model": "deepseek-v4-pro",
		"messages": [
			{"role": "user", "content": [
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
			]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "u1", "name": "Ping"}
			]},
			{"role": "user", "content": [{"type": "text", "text": "hi"}]}
		]
	}`)
	out, _, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	obj := decodeOrFail(t, out)
	msgs := obj["messages"].([]any)
	asst := msgs[1].(map[string]any)
	block := asst["content"].([]any)[0].(map[string]any)
	if block["text"] != "[used Ping tool]" {
		t.Fatalf("text = %q, want %q", block["text"], "[used Ping tool]")
	}
}

func TestRouteVision_DropsToolsAndToolChoice(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`{
		"model": "deepseek-v4-pro",
		"tools": [{"name": "Read"}],
		"tool_choice": {"type": "auto"},
		"messages": [
			{"role": "user", "content": [
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
			]}
		]
	}`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !info.ToolsDropped || !info.ToolChoiceDropped {
		t.Fatalf("expected both dropped flags true: %+v", info)
	}
	obj := decodeOrFail(t, out)
	if _, ok := obj["tools"]; ok {
		t.Fatalf("tools key still present")
	}
	if _, ok := obj["tool_choice"]; ok {
		t.Fatalf("tool_choice key still present")
	}
}

func TestRouteVision_MultiImageMultipleTurns(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`{
		"model": "deepseek-v4-pro",
		"messages": [
			{"role": "user", "content": [
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}},
				{"type": "text", "text": "first"}
			]},
			{"role": "assistant", "content": [{"type": "text", "text": "ok"}]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "x", "content": [
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "BBBB"}}
				]},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "CCCC"}},
				{"type": "text", "text": "what are these"}
			]}
		]
	}`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !info.Routed || info.ImageCount != 3 {
		t.Fatalf("info wrong: %+v", info)
	}
	obj := decodeOrFail(t, out)
	msgs := obj["messages"].([]any)
	last := msgs[2].(map[string]any)
	content := last["content"].([]any)
	// Order: collected during traversal:
	//   - msgs[0] (earlier user) image (AAAA)
	//   - msgs[2] (last user) tool_result-nested image (BBBB)
	//   - msgs[2] (last user) direct image (CCCC)
	// Then flattened text from last user ("what are these").
	if len(content) != 4 {
		t.Fatalf("last user content len = %d (%+v)", len(content), content)
	}
	imgData := func(i int) string {
		blk := content[i].(map[string]any)
		src := blk["source"].(map[string]any)
		return src["data"].(string)
	}
	if imgData(0) != "AAAA" || imgData(1) != "BBBB" || imgData(2) != "CCCC" {
		t.Fatalf("image order wrong: %v %v %v", imgData(0), imgData(1), imgData(2))
	}
	textBlock := content[3].(map[string]any)
	if textBlock["text"] != "what are these" {
		t.Fatalf("trailing text wrong: %+v", textBlock)
	}
}

func TestFormatToolUseLabel(t *testing.T) {
	cases := []struct {
		name string
		tool string
		inp  map[string]any
		want string
	}{
		{
			name: "single string arg",
			tool: "Read",
			inp:  map[string]any{"file_path": "/tmp/img.png"},
			want: "[used Read tool: file_path='/tmp/img.png']",
		},
		{
			name: "multiple args sorted by key",
			tool: "Edit",
			inp:  map[string]any{"new_string": "B", "old_string": "A"},
			want: "[used Edit tool: new_string='B', old_string='A']",
		},
		{
			name: "no args",
			tool: "Ping",
			inp:  nil,
			want: "[used Ping tool]",
		},
		{
			name: "numeric arg",
			tool: "Sleep",
			inp:  map[string]any{"seconds": float64(5)},
			want: "[used Sleep tool: seconds=5]",
		},
		{
			name: "boolean arg",
			tool: "Toggle",
			inp:  map[string]any{"on": true},
			want: "[used Toggle tool: on=True]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatToolUseLabel(tc.tool, tc.inp)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRouteVision_RoundTripModelOverride verifies the model override
// is the *only* mutation when an already-vision-bound request flies
// through (no images, no tools) — i.e. routing decision is image-gated,
// not model-gated.
func TestRouteVision_NoImageEvenWithVisionModel(t *testing.T) {
	cfg := &Config{VisionModel: "deepseek-chat"}
	in := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	out, info, err := routeVision(in, cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if info.Routed {
		t.Fatalf("expected Routed=false (no images)")
	}
	// Body untouched (we never re-marshalled).
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("body mutated: %s", out)
	}
}
