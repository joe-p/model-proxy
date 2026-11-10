package responsesbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/joe-p/model-proxy/internal/provider"
)

func sseResponse(contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestTranslateRequest(t *testing.T) {
	input := `{
		"model": "gpt-5",
		"messages": [
			{"role": "system", "content": "be nice"},
			{"role": "user", "content": [
				{"type": "text", "text": "hello"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AA", "detail": "low"}}
			]},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":\"x\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "result"}
		],
		"tools": [{"type": "function", "function": {"name": "lookup", "description": "d", "parameters": {"type": "object"}, "strict": true}}],
		"tool_choice": {"type": "function", "function": {"name": "lookup"}},
		"response_format": {"type": "json_schema", "json_schema": {"name": "out", "schema": {"type": "object"}, "strict": true}},
		"max_tokens": 100,
		"temperature": 0.2,
		"stream": false,
		"stream_options": {"include_usage": true},
		"n": 2,
		"stop": ["x"]
	}`

	body, stream, err := translateRequest([]byte(input))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	if stream {
		t.Fatal("stream = true, want false")
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal translated body: %v", err)
	}
	for _, key := range []string{"messages", "stream_options", "n", "stop"} {
		if _, exists := out[key]; exists {
			t.Errorf("translated request still has %q", key)
		}
	}
	if got := string(out["stream"]); got != "true" {
		t.Errorf("stream = %s", got)
	}
	if got := string(out["store"]); got != "false" {
		t.Errorf("store = %s", got)
	}
	if got := string(out["instructions"]); got != `"be nice"` {
		t.Errorf("instructions = %s", got)
	}
	if got := string(out["max_output_tokens"]); got != "100" {
		t.Errorf("max_output_tokens = %s", got)
	}
	if _, exists := out["temperature"]; exists {
		t.Errorf("translated request still has temperature: %s", out["temperature"])
	}

	var inputItems []map[string]json.RawMessage
	if err := json.Unmarshal(out["input"], &inputItems); err != nil {
		t.Fatalf("input: %v", err)
	}
	if len(inputItems) != 3 {
		t.Fatalf("input item count = %d, want 3", len(inputItems))
	}
	if got := string(inputItems[0]["role"]); got != `"user"` {
		t.Errorf("user role = %s", got)
	}
	if !strings.Contains(string(inputItems[0]["content"]), `"type":"input_text"`) ||
		!strings.Contains(string(inputItems[0]["content"]), `"type":"input_image"`) {
		t.Errorf("user content = %s", inputItems[0]["content"])
	}
	if got := string(inputItems[1]["type"]); got != `"function_call"` {
		t.Errorf("assistant item type = %s", got)
	}
	if got := string(inputItems[2]["type"]); got != `"function_call_output"` {
		t.Errorf("tool item type = %s", got)
	}
	if got := string(inputItems[2]["output"]); got != `"result"` {
		t.Errorf("tool output = %s", got)
	}

	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(out["tools"], &tools); err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 1 || string(tools[0]["type"]) != `"function"` || string(tools[0]["name"]) != `"lookup"` {
		t.Errorf("tools = %s", out["tools"])
	}
	if _, nested := tools[0]["function"]; nested {
		t.Errorf("tool still nested: %s", out["tools"])
	}

	var toolChoice map[string]json.RawMessage
	if err := json.Unmarshal(out["tool_choice"], &toolChoice); err != nil {
		t.Fatalf("tool_choice: %v", err)
	}
	if string(toolChoice["type"]) != `"function"` || string(toolChoice["name"]) != `"lookup"` {
		t.Errorf("tool_choice = %s", out["tool_choice"])
	}

	var text map[string]json.RawMessage
	if err := json.Unmarshal(out["text"], &text); err != nil {
		t.Fatalf("text: %v", err)
	}
	var format map[string]json.RawMessage
	if err := json.Unmarshal(text["format"], &format); err != nil {
		t.Fatalf("text.format: %v", err)
	}
	if string(format["type"]) != `"json_schema"` || string(format["name"]) != `"out"` {
		t.Errorf("text.format = %s", text["format"])
	}
}

func TestChatCompletionsBuffersResponsesStream(t *testing.T) {
	events := `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5","created_at":1700000000}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"Hel"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"lo"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-5","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]},{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}

`

	var gotRequest provider.Request
	response, err := ChatCompletions(context.Background(), provider.Request{
		UpstreamModel: "gpt-5",
		Body:          []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`),
	}, func(_ context.Context, req provider.Request) (*http.Response, error) {
		gotRequest = req
		return sseResponse("text/event-stream", events), nil
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	defer response.Body.Close()

	if !gotRequest.Stream || gotRequest.Accept != "text/event-stream" {
		t.Errorf("upstream request stream=%v accept=%q", gotRequest.Stream, gotRequest.Accept)
	}
	var upstreamBody map[string]json.RawMessage
	if err := json.Unmarshal(gotRequest.Body, &upstreamBody); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if string(upstreamBody["stream"]) != "true" {
		t.Errorf("upstream stream = %s", upstreamBody["stream"])
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		t.Fatalf("decode completion: %v (body %s)", err, body)
	}
	if completion.Object != "chat.completion" {
		t.Errorf("object = %q", completion.Object)
	}
	if completion.Choices[0].Message.Content != "Hello" {
		t.Errorf("content = %q", completion.Choices[0].Message.Content)
	}
	if completion.Choices[0].Message.ReasoningContent != "think" {
		t.Errorf("reasoning = %q", completion.Choices[0].Message.ReasoningContent)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", completion.Choices[0].FinishReason)
	}
	if completion.Usage.PromptTokens != 3 || completion.Usage.CompletionTokens != 2 || completion.Usage.TotalTokens != 5 {
		t.Errorf("usage = %+v", completion.Usage)
	}
}

func TestChatCompletionsStreamsTranslatedDeltas(t *testing.T) {
	events := `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5","created_at":1700000000}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"Hi"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hi"}]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"q\""}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":":\"x\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-5","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}

`

	response, err := ChatCompletions(context.Background(), provider.Request{
		UpstreamModel: "gpt-5",
		Body:          []byte(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}, func(_ context.Context, req provider.Request) (*http.Response, error) {
		return sseResponse("text/event-stream", events), nil
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("stream missing [DONE]: %s", body)
	}

	var content strings.Builder
	var arguments strings.Builder
	var toolID, toolName string
	var finishReason string
	var usage *chatUsage
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("decode chunk: %v (%s)", err, line)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				content.WriteString(*choice.Delta.Content)
			}
			for _, call := range choice.Delta.ToolCalls {
				if call.ID != "" {
					toolID = call.ID
				}
				if call.Function != nil {
					if call.Function.Name != "" {
						toolName = call.Function.Name
					}
					if call.Function.Arguments != nil {
						arguments.WriteString(*call.Function.Arguments)
					}
				}
			}
			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
		}
	}

	if content.String() != "Hi" {
		t.Errorf("content = %q", content.String())
	}
	if toolID != "call_1" || toolName != "lookup" {
		t.Errorf("tool id/name = %q/%q", toolID, toolName)
	}
	if arguments.String() != `{"q":"x"}` {
		t.Errorf("arguments = %q", arguments.String())
	}
	if finishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", finishReason)
	}
	if usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestChatCompletionsPassesUpstreamError(t *testing.T) {
	const upstreamBody = `{"detail":"Stream must be set to true"}`
	response, err := ChatCompletions(context.Background(), provider.Request{
		Body: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`),
	}, func(_ context.Context, req provider.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		}, nil
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if string(body) != upstreamBody {
		t.Errorf("body = %s", body)
	}
}

func TestChatCompletionsSynthesizesStreamFromJSON(t *testing.T) {
	const upstreamBody = `{"id":"resp_1","object":"response","created_at":1700000000,"status":"completed","model":"gpt-5","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	response, err := ChatCompletions(context.Background(), provider.Request{
		UpstreamModel: "gpt-5",
		Body:          []byte(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}, func(_ context.Context, req provider.Request) (*http.Response, error) {
		return sseResponse("application/json", upstreamBody), nil
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	defer response.Body.Close()

	body, _ := io.ReadAll(response.Body)
	text := string(body)
	if !strings.Contains(text, `"content":"hello"`) {
		t.Errorf("stream missing content: %s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Errorf("stream missing finish reason: %s", text)
	}
	if !strings.Contains(text, `"prompt_tokens":3`) {
		t.Errorf("stream missing usage: %s", text)
	}
	if !strings.Contains(text, "data: [DONE]") {
		t.Errorf("stream missing [DONE]: %s", text)
	}
}
