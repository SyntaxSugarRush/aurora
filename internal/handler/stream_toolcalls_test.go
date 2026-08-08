package handler

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// parseSSEChunks 把 SSE 输出拆成 [DONE] 之前的 chunk 列表。
func parseSSEChunks(t *testing.T, body string) ([]map[string]interface{}, bool) {
	t.Helper()
	var chunks []map[string]interface{}
	sawDone := false
	for _, line := range sseDataLines(body) {
		if line == "[DONE]" {
			sawDone = true
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("invalid SSE chunk %q: %v", line, err)
		}
		chunks = append(chunks, m)
	}
	return chunks, sawDone
}

func choiceOf(t *testing.T, chunk map[string]interface{}) map[string]interface{} {
	t.Helper()
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("chunk has no choices: %v", chunk)
	}
	return choices[0].(map[string]interface{})
}

func TestSynthesizeToolCallStreamEmitsCanonicalSequence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	calls := []officialtypes.ToolCall{
		{Index: 0, ID: "call_aaa", Type: "function", Function: officialtypes.ToolCallFunc{Name: "bash", Arguments: `{"command":"ls"}`}},
		{Index: 1, ID: "call_bbb", Type: "function", Function: officialtypes.ToolCallFunc{Name: "read_file", Arguments: `{"path":"/tmp/x"}`}},
	}
	synthesizeToolCallStream(c, "Let me check.", calls, "gpt-5", "conv-1", 100, true, time.Now())

	body := writer.Body.String()
	if ct := writer.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if strings.Contains(body, "<tool_call>") {
		t.Fatalf("SSE output leaks raw <tool_call> protocol: %s", body)
	}

	chunks, sawDone := parseSSEChunks(t, body)
	if !sawDone {
		t.Fatalf("missing [DONE]: %s", body)
	}
	// role + text + 2*(head+args) + finish + usage = 8
	if len(chunks) != 8 {
		t.Fatalf("chunk count = %d, want 8: %s", len(chunks), body)
	}

	// 0: role chunk
	delta0 := choiceOf(t, chunks[0])["delta"].(map[string]interface{})
	if delta0["role"] != "assistant" {
		t.Fatalf("first chunk delta.role = %#v", delta0["role"])
	}

	// 1: text delta
	delta1 := choiceOf(t, chunks[1])["delta"].(map[string]interface{})
	if delta1["content"] != "Let me check." {
		t.Fatalf("text delta content = %#v", delta1["content"])
	}

	// 2..5: tool_call deltas, aggregated per index like the OpenAI SDK does
	type agg struct{ id, name, args string }
	aggs := map[float64]*agg{}
	for _, chunk := range chunks[2:6] {
		delta := choiceOf(t, chunk)["delta"].(map[string]interface{})
		tcs, ok := delta["tool_calls"].([]interface{})
		if !ok || len(tcs) != 1 {
			t.Fatalf("delta.tool_calls missing: %v", delta)
		}
		tc := tcs[0].(map[string]interface{})
		idx := tc["index"].(float64)
		a := aggs[idx]
		if a == nil {
			a = &agg{}
			aggs[idx] = a
		}
		if id, ok := tc["id"].(string); ok {
			a.id = id
			if tc["type"] != "function" {
				t.Fatalf("head delta type = %#v", tc["type"])
			}
		}
		fn := tc["function"].(map[string]interface{})
		if name, ok := fn["name"].(string); ok {
			a.name = name
		}
		if args, ok := fn["arguments"].(string); ok {
			a.args += args
		}
	}
	if len(aggs) != 2 {
		t.Fatalf("aggregated tool calls = %d, want 2", len(aggs))
	}
	if aggs[0].id != "call_aaa" || aggs[0].name != "bash" {
		t.Fatalf("call 0 = %+v", aggs[0])
	}
	if aggs[1].id != "call_bbb" || aggs[1].name != "read_file" {
		t.Fatalf("call 1 = %+v", aggs[1])
	}
	for idx, a := range aggs {
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(a.args), &parsed); err != nil {
			t.Fatalf("call %v arguments not valid JSON: %q", idx, a.args)
		}
	}

	// 6: finish_reason=tool_calls
	finish := choiceOf(t, chunks[6])["finish_reason"]
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %#v, want tool_calls", finish)
	}

	// 7: usage chunk (choices empty, usage present)
	if usage, ok := chunks[7]["usage"].(map[string]interface{}); !ok || usage["prompt_tokens"].(float64) != 100 {
		t.Fatalf("usage chunk malformed: %v", chunks[7])
	}
	if choices := chunks[7]["choices"].([]interface{}); len(choices) != 0 {
		t.Fatalf("usage chunk choices not empty: %v", choices)
	}
}

func TestSynthesizeToolCallStreamPlainTextOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	synthesizeToolCallStream(c, "plain answer", nil, "gpt-5", "", 10, false, time.Now())

	chunks, sawDone := parseSSEChunks(t, writer.Body.String())
	if !sawDone {
		t.Fatal("missing [DONE]")
	}
	// role + text + finish = 3 (no usage: includeUsage=false)
	if len(chunks) != 3 {
		t.Fatalf("chunk count = %d, want 3: %s", len(chunks), writer.Body.String())
	}
	if finish := choiceOf(t, chunks[2])["finish_reason"]; finish != "stop" {
		t.Fatalf("finish_reason = %#v, want stop", finish)
	}
}

func TestSynthesizeToolCallStreamNoTextLeakWithoutTags(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	// RecoverFromText 兜底路径:正文为空,只有 tool_calls
	calls := []officialtypes.ToolCall{
		{Index: 0, ID: "call_x", Type: "function", Function: officialtypes.ToolCallFunc{Name: "bash", Arguments: `{}`}},
	}
	synthesizeToolCallStream(c, "", calls, "gpt-5", "", 5, false, time.Now())

	chunks, _ := parseSSEChunks(t, writer.Body.String())
	// role + head + args + finish = 4;无 text delta
	if len(chunks) != 4 {
		t.Fatalf("chunk count = %d, want 4: %s", len(chunks), writer.Body.String())
	}
	delta1 := choiceOf(t, chunks[1])["delta"].(map[string]interface{})
	if _, hasContent := delta1["content"]; hasContent {
		t.Fatalf("unexpected content delta when text empty: %v", delta1)
	}
}
