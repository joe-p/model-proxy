// Package responsesbridge adapts an upstream that only serves the Responses
// API to the Chat Completions contract the proxy exposes to clients. It is
// used by the Codex provider, whose ChatGPT backend does not implement
// /chat/completions.
package responsesbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joe-p/model-proxy/internal/provider"
)

// FetchResponses calls the upstream Responses API. It matches the signature of
// openaicompat.Provider.Responses so a provider can pass its transport in
// directly.
type FetchResponses func(ctx context.Context, req provider.Request) (*http.Response, error)

// ChatCompletions translates a Chat Completions request into a Responses API
// request, calls the upstream, and translates the response back into the Chat
// Completions dialect. The upstream is always asked to stream because the
// ChatGPT/Codex backend rejects non-streaming Responses requests; a request
// from a non-streaming client is buffered and returned as plain JSON.
func ChatCompletions(ctx context.Context, req provider.Request, fetch FetchResponses) (*http.Response, error) {
	upstreamBody, clientStream, err := translateRequest(req.Body)
	if err != nil {
		return errorResponse(http.StatusBadRequest, err.Error()), nil
	}

	upstreamReq := req
	upstreamReq.Body = upstreamBody
	upstreamReq.Stream = true
	upstreamReq.Accept = "text/event-stream"

	resp, err := fetch(ctx, upstreamReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		// Keep the upstream error body so the server can rewrite it into the
		// proxy's error envelope.
		return resp, nil
	}

	// The Codex backend streams Responses events without a Content-Type
	// header, so only an explicit JSON content type selects the buffered path.
	if isJSONResponse(resp.Header.Get("Content-Type")) {
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return errorResponse(http.StatusBadGateway, readErr.Error()), nil
		}
		var final responsesResponse
		if err := json.Unmarshal(body, &final); err != nil {
			return errorResponse(http.StatusBadGateway, "upstream returned an invalid Responses body"), nil
		}
		if final.Status == "failed" || (final.Error != nil && final.Error.Message != "") {
			message := "upstream Responses request failed"
			if final.Error != nil && final.Error.Message != "" {
				message = final.Error.Message
			}
			return errorResponse(http.StatusBadGateway, message), nil
		}
		return translatedResponse(resp.Header, &final, req.UpstreamModel, clientStream)
	}

	if clientStream {
		return streamingResponse(resp, req.UpstreamModel), nil
	}

	final, err := readFinalResponse(resp.Body)
	resp.Body.Close()
	if err != nil {
		return errorResponse(http.StatusBadGateway, err.Error()), nil
	}
	return translatedResponse(resp.Header, final, req.UpstreamModel, false)
}

func translatedResponse(upstreamHeader http.Header, final *responsesResponse, fallbackModel string, stream bool) (*http.Response, error) {
	if stream {
		body, err := synthesizedStream(final, fallbackModel)
		if err != nil {
			return errorResponse(http.StatusBadGateway, err.Error()), nil
		}
		header := cloneHeader(upstreamHeader)
		header.Set("Content-Type", "text/event-stream")
		header.Del("Content-Length")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     header,
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	}

	body, err := chatCompletionBody(final, fallbackModel)
	if err != nil {
		return errorResponse(http.StatusBadGateway, err.Error()), nil
	}
	header := cloneHeader(upstreamHeader)
	header.Set("Content-Type", "application/json")
	header.Del("Content-Length")
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func streamingResponse(upstream *http.Response, fallbackModel string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		defer upstream.Body.Close()
		translator := newStreamTranslator(pw, fallbackModel)
		err := readEvents(upstream.Body, translator.handle)
		if err == nil {
			err = translator.finish()
		}
		if err != nil {
			translator.writeError(err.Error())
		}
		if translator.err != nil {
			_ = pw.CloseWithError(translator.err)
			return
		}
		_ = pw.Close()
	}()

	header := cloneHeader(upstream.Header)
	header.Set("Content-Type", "text/event-stream")
	header.Del("Content-Length")
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     header,
		Body:       pr,
	}
}

func errorResponse(status int, message string) *http.Response {
	body, _ := json.Marshal(map[string]string{"detail": message})
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func isJSONResponse(contentType string) bool {
	contentType = strings.ToLower(contentType)
	return strings.Contains(contentType, "application/json") || strings.Contains(contentType, "+json")
}

// -----------------------------------------------------------------------------
// Chat Completions -> Responses request translation
// -----------------------------------------------------------------------------

var responsesPassthroughFields = []string{
	"include",
	"instructions",
	"max_output_tokens",
	"max_tool_calls",
	"prompt_cache_key",
	"reasoning",
	"safety_identifier",
	"store",
	"text",
}

// The ChatGPT/Codex backend rejects most sampling and attribution parameters
// that Chat Completions clients commonly send. Only fields the Responses API
// and the Codex backend both accept are passed through; the rest are dropped.
var chatPassthroughFields = []string{
	"parallel_tool_calls",
}

type chatMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Name         string          `json:"name"`
	ToolCalls    []chatToolCall  `json:"tool_calls"`
	ToolCallID   string          `json:"tool_call_id"`
	FunctionCall *chatFunction   `json:"function_call"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func translateRequest(body []byte) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, false, fmt.Errorf("request body must be a JSON object")
	}

	stream := false
	if raw, ok := fields["stream"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &stream); err != nil {
			return nil, false, fmt.Errorf("request field \"stream\" must be a boolean")
		}
	}

	rawMessages, ok := fields["messages"]
	if !ok || isNull(rawMessages) {
		return nil, false, fmt.Errorf("request field \"messages\" is required")
	}
	var messages []chatMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return nil, false, fmt.Errorf("request field \"messages\" must be an array")
	}

	instructions, input, err := convertMessages(messages)
	if err != nil {
		return nil, false, err
	}

	out := make(map[string]any, len(fields)+4)

	for _, key := range responsesPassthroughFields {
		if raw, ok := fields[key]; ok && !isNull(raw) {
			out[key] = raw
		}
	}
	for _, key := range chatPassthroughFields {
		if raw, ok := fields[key]; ok && !isNull(raw) {
			out[key] = raw
		}
	}

	if raw, ok := fields["model"]; ok && !isNull(raw) {
		out["model"] = raw
	}
	if _, exists := out["instructions"]; !exists && instructions != "" {
		out["instructions"] = instructions
	}
	out["input"] = input

	if raw, ok := fields["tools"]; ok && !isNull(raw) {
		converted, err := convertTools(raw)
		if err != nil {
			return nil, false, err
		}
		out["tools"] = converted
	} else if raw, ok := fields["functions"]; ok && !isNull(raw) {
		converted, err := convertLegacyFunctions(raw)
		if err != nil {
			return nil, false, err
		}
		out["tools"] = converted
	}

	if raw, ok := fields["tool_choice"]; ok && !isNull(raw) {
		out["tool_choice"] = convertToolChoice(raw)
	} else if raw, ok := fields["function_call"]; ok && !isNull(raw) {
		out["tool_choice"] = convertLegacyFunctionCall(raw)
	}

	text := map[string]any{}
	if raw, ok := fields["text"]; ok && !isNull(raw) {
		_ = json.Unmarshal(raw, &text)
		if text == nil {
			text = map[string]any{}
		}
	}
	if raw, ok := fields["response_format"]; ok && !isNull(raw) {
		if format := convertResponseFormat(raw); format != nil {
			if _, exists := text["format"]; !exists {
				text["format"] = format
			}
		}
	}
	if raw, ok := fields["verbosity"]; ok && !isNull(raw) {
		if _, exists := text["verbosity"]; !exists {
			text["verbosity"] = raw
		}
	}
	if len(text) > 0 {
		out["text"] = text
	}

	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if raw, ok := fields[key]; ok && !isNull(raw) {
			if _, exists := out["max_output_tokens"]; !exists {
				out["max_output_tokens"] = raw
			}
			break
		}
	}

	if raw, ok := fields["reasoning_effort"]; ok && !isNull(raw) {
		reasoning := map[string]any{}
		if existing, ok := out["reasoning"].(json.RawMessage); ok {
			_ = json.Unmarshal(existing, &reasoning)
			if reasoning == nil {
				reasoning = map[string]any{}
			}
		}
		if _, exists := reasoning["effort"]; !exists {
			reasoning["effort"] = raw
		}
		out["reasoning"] = reasoning
	}

	// The Codex backend requires streaming and rejects stored responses.
	out["stream"] = true
	if _, exists := out["store"]; !exists {
		out["store"] = false
	}

	translated, err := json.Marshal(out)
	if err != nil {
		return nil, false, err
	}
	return translated, stream, nil
}

func convertMessages(messages []chatMessage) (string, []any, error) {
	var instructions []string
	input := make([]any, 0, len(messages))

	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "system", "developer":
			if text := contentText(message.Content); text != "" {
				instructions = append(instructions, text)
			}
		case "assistant":
			parts, err := convertContent(message.Content, "output_text")
			if err != nil {
				return "", nil, err
			}
			if len(parts) > 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": parts,
				})
			}
			for _, call := range message.ToolCalls {
				input = append(input, functionCallItem(call.ID, call.Function.Name, call.Function.Arguments))
			}
			if message.FunctionCall != nil {
				input = append(input, functionCallItem("call_"+message.FunctionCall.Name, message.FunctionCall.Name, message.FunctionCall.Arguments))
			}
		case "tool":
			callID := strings.TrimSpace(message.ToolCallID)
			if callID == "" {
				return "", nil, fmt.Errorf("tool message is missing tool_call_id")
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  contentText(message.Content),
			})
		case "function":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": strings.TrimSpace(message.Name),
				"output":  contentText(message.Content),
			})
		default:
			parts, err := convertContent(message.Content, "input_text")
			if err != nil {
				return "", nil, err
			}
			input = append(input, map[string]any{
				"type":    "message",
				"role":    role,
				"content": parts,
			})
		}
	}

	return strings.Join(instructions, "\n\n"), input, nil
}

func functionCallItem(callID, name, arguments string) map[string]any {
	return map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
	}
}

type chatContentPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	Refusal    string          `json:"refusal"`
	ImageURL   json.RawMessage `json:"image_url"`
	InputImage json.RawMessage `json:"input_image"`
	InputAudio json.RawMessage `json:"input_audio"`
	File       json.RawMessage `json:"file"`
}

func convertContent(raw json.RawMessage, textType string) ([]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return nil, nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": textType, "text": text}}, nil
	}

	var parts []chatContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("message content must be a string or an array of content parts")
	}

	converted := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
			if part.Text != "" {
				converted = append(converted, map[string]any{"type": textType, "text": part.Text})
			}
		case "refusal":
			if textType == "output_text" && part.Refusal != "" {
				converted = append(converted, map[string]any{"type": "refusal", "refusal": part.Refusal})
			}
		case "image_url":
			if textType == "input_text" {
				if image, ok := convertImageURL(part.ImageURL); ok {
					converted = append(converted, image)
				}
			}
		case "input_image":
			if textType == "input_text" && !isNull(part.InputImage) {
				converted = append(converted, part.InputImage)
			}
		case "input_audio":
			if textType == "input_text" && !isNull(part.InputAudio) {
				converted = append(converted, map[string]any{"type": "input_audio", "input_audio": part.InputAudio})
			}
		case "file":
			if textType == "input_text" {
				if file, ok := convertFile(part.File); ok {
					converted = append(converted, file)
				}
			}
		case "input_file":
			if textType == "input_text" && !isNull(part.File) {
				converted = append(converted, part.File)
			}
		}
	}
	return converted, nil
}

func convertImageURL(raw json.RawMessage) (map[string]any, bool) {
	if isNull(raw) {
		return nil, false
	}
	var url string
	if err := json.Unmarshal(raw, &url); err == nil && url != "" {
		return map[string]any{"type": "input_image", "image_url": url}, true
	}
	var object struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &object); err != nil || object.URL == "" {
		return nil, false
	}
	out := map[string]any{"type": "input_image", "image_url": object.URL}
	if object.Detail != "" {
		out["detail"] = object.Detail
	}
	return out, true
}

func convertFile(raw json.RawMessage) (map[string]any, bool) {
	if isNull(raw) {
		return nil, false
	}
	var object struct {
		FileID   string `json:"file_id"`
		FileData string `json:"file_data"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false
	}
	out := map[string]any{"type": "input_file"}
	if object.FileID != "" {
		out["file_id"] = object.FileID
	}
	if object.FileData != "" {
		out["file_data"] = object.FileData
	}
	if object.Filename != "" {
		out["filename"] = object.Filename
	}
	if len(out) == 1 {
		return nil, false
	}
	return out, true
}

// contentText flattens chat content into plain text. It is used for system
// instructions and tool output, which the Responses API represents as strings.
func contentText(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []chatContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
			if builder.Len() > 0 && part.Text != "" {
				builder.WriteString("\n")
			}
			builder.WriteString(part.Text)
		case "refusal":
			if builder.Len() > 0 && part.Refusal != "" {
				builder.WriteString("\n")
			}
			builder.WriteString(part.Refusal)
		}
	}
	return builder.String()
}

func convertTools(raw json.RawMessage) ([]any, error) {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("request field \"tools\" must be an array")
	}
	converted := make([]any, 0, len(tools))
	for _, tool := range tools {
		converted = append(converted, convertTool(tool))
	}
	return converted, nil
}

func convertTool(raw json.RawMessage) any {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return raw
	}
	typ, _ := rawString(object["type"])
	if typ != "function" {
		// Already in Responses form, or a built-in tool.
		return raw
	}
	function, ok := object["function"]
	if !ok || isNull(function) {
		return raw
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(function, &details); err != nil {
		return raw
	}
	out := map[string]any{"type": "function"}
	for _, key := range []string{"name", "description", "parameters", "strict"} {
		if value, ok := details[key]; ok && !isNull(value) {
			out[key] = value
		}
	}
	return out
}

func convertLegacyFunctions(raw json.RawMessage) ([]any, error) {
	var functions []json.RawMessage
	if err := json.Unmarshal(raw, &functions); err != nil {
		return nil, fmt.Errorf("request field \"functions\" must be an array")
	}
	converted := make([]any, 0, len(functions))
	for _, function := range functions {
		var details map[string]json.RawMessage
		if err := json.Unmarshal(function, &details); err != nil {
			continue
		}
		out := map[string]any{"type": "function"}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, ok := details[key]; ok && !isNull(value) {
				out[key] = value
			}
		}
		converted = append(converted, out)
	}
	return converted, nil
}

func convertToolChoice(raw json.RawMessage) any {
	var choice string
	if err := json.Unmarshal(raw, &choice); err == nil {
		return choice
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return raw
	}
	if function, ok := object["function"]; ok && !isNull(function) {
		var details map[string]json.RawMessage
		if err := json.Unmarshal(function, &details); err == nil {
			out := map[string]any{"type": "function"}
			if name, ok := details["name"]; ok && !isNull(name) {
				out["name"] = name
			}
			return out
		}
	}
	return raw
}

func convertLegacyFunctionCall(raw json.RawMessage) any {
	var choice string
	if err := json.Unmarshal(raw, &choice); err == nil {
		return choice
	}
	var object struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.Name != "" {
		return map[string]any{"type": "function", "name": object.Name}
	}
	return raw
}

func convertResponseFormat(raw json.RawMessage) any {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	typ, _ := rawString(object["type"])
	switch typ {
	case "json_schema":
		schema, ok := object["json_schema"]
		if !ok || isNull(schema) {
			return nil
		}
		var details map[string]json.RawMessage
		if err := json.Unmarshal(schema, &details); err != nil {
			return nil
		}
		out := map[string]any{"type": "json_schema"}
		for _, key := range []string{"name", "description", "schema", "strict"} {
			if value, ok := details[key]; ok && !isNull(value) {
				out[key] = value
			}
		}
		return out
	case "":
		return nil
	default:
		return map[string]any{"type": typ}
	}
}

func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || isNull(raw) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// -----------------------------------------------------------------------------
// Responses -> Chat Completions response translation
// -----------------------------------------------------------------------------

type responsesResponse struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Model             string             `json:"model"`
	Output            []responseItem     `json:"output"`
	Usage             *responsesUsage    `json:"usage"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
	Error             *responseError     `json:"error"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

type responseError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}

type responseItem struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Role      string            `json:"role"`
	Status    string            `json:"status"`
	Content   []responseContent `json:"content"`
	Summary   []responseContent `json:"summary"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
}

type responseContent struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Refusal     string            `json:"refusal"`
	Annotations []json.RawMessage `json:"annotations"`
}

type responsesUsage struct {
	InputTokens         int64                `json:"input_tokens"`
	OutputTokens        int64                `json:"output_tokens"`
	TotalTokens         int64                `json:"total_tokens"`
	InputTokensDetails  *inputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails *outputTokensDetails `json:"output_tokens_details"`
}

type inputTokensDetails struct {
	CachedTokens     *int64 `json:"cached_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}

type outputTokensDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens"`
}

type chatUsage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     *chatPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *chatCompletionDetails   `json:"completion_tokens_details,omitempty"`
}

type chatPromptTokensDetails struct {
	CachedTokens     *int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
}

type chatCompletionDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

type chatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   *chatUsage             `json:"usage,omitempty"`
}

type chatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      chatCompletionMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
	Logprobs     any                   `json:"logprobs"`
}

type chatCompletionMessage struct {
	Role             string            `json:"role"`
	Content          *string           `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	Refusal          *string           `json:"refusal,omitempty"`
	Annotations      []json.RawMessage `json:"annotations,omitempty"`
	ToolCalls        []chatToolCallOut `json:"tool_calls,omitempty"`
}

type chatToolCallOut struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function chatFunctionOut `json:"function"`
}

type chatFunctionOut struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *chatUsage        `json:"usage,omitempty"`
}

type chatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chatDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Refusal          *string         `json:"refusal,omitempty"`
	ToolCalls        []chatToolDelta `json:"tool_calls,omitempty"`
}

type chatToolDelta struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function *chatFunctionDelta `json:"function,omitempty"`
}

type chatFunctionDelta struct {
	Name      string  `json:"name,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
}

func chatCompletionBody(final *responsesResponse, fallbackModel string) ([]byte, error) {
	response := chatCompletionFromResponses(final, fallbackModel)
	return json.Marshal(response)
}

func chatCompletionFromResponses(final *responsesResponse, fallbackModel string) chatCompletionResponse {
	created := final.CreatedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	model := final.Model
	if model == "" {
		model = fallbackModel
	}
	id := final.ID
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}

	var text, reasoning, refusal strings.Builder
	var annotations []json.RawMessage
	toolCalls := make([]chatToolCallOut, 0)
	for _, item := range final.Output {
		switch item.Type {
		case "message":
			text.WriteString(messageText(&item))
			if value, ok := messageRefusal(&item); ok {
				refusal.WriteString(value)
			}
			annotations = append(annotations, messageAnnotations(&item)...)
		case "reasoning":
			reasoning.WriteString(reasoningText(&item))
		case "function_call":
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			toolCalls = append(toolCalls, chatToolCallOut{
				ID:   callID,
				Type: "function",
				Function: chatFunctionOut{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		}
	}

	message := chatCompletionMessage{
		Role:             "assistant",
		ReasoningContent: reasoning.String(),
	}
	if text.Len() > 0 {
		value := text.String()
		message.Content = &value
	} else if len(toolCalls) == 0 && refusal.Len() == 0 {
		value := ""
		message.Content = &value
	}
	if refusal.Len() > 0 {
		value := refusal.String()
		message.Refusal = &value
	}
	if len(annotations) > 0 {
		message.Annotations = annotations
	}
	if len(toolCalls) > 0 {
		message.ToolCalls = toolCalls
	}

	return chatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []chatCompletionChoice{{
			Index:        0,
			Message:      message,
			FinishReason: finishReason(final, len(toolCalls) > 0),
			Logprobs:     nil,
		}},
		Usage: chatUsageFromResponses(final.Usage),
	}
}

func finishReason(final *responsesResponse, hasToolCalls bool) string {
	if final.Status == "incomplete" && final.IncompleteDetails != nil {
		switch final.IncompleteDetails.Reason {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		}
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}

func chatUsageFromResponses(in *responsesUsage) *chatUsage {
	if in == nil {
		return nil
	}
	total := in.TotalTokens
	if total == 0 && (in.InputTokens != 0 || in.OutputTokens != 0) {
		total = in.InputTokens + in.OutputTokens
	}
	out := &chatUsage{
		PromptTokens:     in.InputTokens,
		CompletionTokens: in.OutputTokens,
		TotalTokens:      total,
	}
	if in.InputTokensDetails != nil {
		out.PromptTokensDetails = &chatPromptTokensDetails{
			CachedTokens:     in.InputTokensDetails.CachedTokens,
			CacheWriteTokens: in.InputTokensDetails.CacheWriteTokens,
		}
	}
	if in.OutputTokensDetails != nil {
		out.CompletionTokensDetails = &chatCompletionDetails{
			ReasoningTokens: in.OutputTokensDetails.ReasoningTokens,
		}
	}
	return out
}

func messageText(item *responseItem) string {
	var builder strings.Builder
	for _, content := range item.Content {
		switch content.Type {
		case "output_text", "text":
			builder.WriteString(content.Text)
		}
	}
	return builder.String()
}

func messageRefusal(item *responseItem) (string, bool) {
	for _, content := range item.Content {
		if content.Type == "refusal" {
			return content.Refusal, true
		}
	}
	return "", false
}

func messageAnnotations(item *responseItem) []json.RawMessage {
	var annotations []json.RawMessage
	for _, content := range item.Content {
		if content.Type == "output_text" || content.Type == "text" {
			annotations = append(annotations, content.Annotations...)
		}
	}
	return annotations
}

func reasoningText(item *responseItem) string {
	var builder strings.Builder
	for _, summary := range item.Summary {
		builder.WriteString(summary.Text)
	}
	for _, content := range item.Content {
		switch content.Type {
		case "reasoning_text", "text":
			builder.WriteString(content.Text)
		}
	}
	return builder.String()
}

// -----------------------------------------------------------------------------
// Responses SSE translation
// -----------------------------------------------------------------------------

type responsesEvent struct {
	Type        string             `json:"type"`
	Response    *responsesResponse `json:"response"`
	Error       *responseError     `json:"error"`
	OutputIndex int                `json:"output_index"`
	ItemID      string             `json:"item_id"`
	Item        *responseItem      `json:"item"`
	Delta       string             `json:"delta"`
}

func readEvents(r io.Reader, handle func(responsesEvent) error) error {
	reader := bufio.NewReader(r)
	var data []byte
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if len(trimmed) == 0 {
				if len(data) > 0 {
					if dispatchErr := dispatchEvent(data, handle); dispatchErr != nil {
						return dispatchErr
					}
					data = data[:0]
				}
			} else if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimPrefix(trimmed[len("data:"):], []byte(" "))
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, payload...)
			}
		}
		if err != nil {
			if err == io.EOF {
				if len(data) > 0 {
					return dispatchEvent(data, handle)
				}
				return nil
			}
			return err
		}
	}
}

func dispatchEvent(data []byte, handle func(responsesEvent) error) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return nil
	}
	var event responsesEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil
	}
	if event.Type == "" {
		return nil
	}
	return handle(event)
}

func readFinalResponse(r io.Reader) (*responsesResponse, error) {
	var final *responsesResponse
	err := readEvents(r, func(event responsesEvent) error {
		switch event.Type {
		case "response.completed", "response.incomplete":
			if event.Response != nil {
				final = event.Response
			}
		case "response.failed", "error":
			return eventError(event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if final == nil {
		return nil, errors.New("upstream stream ended before response.completed")
	}
	return final, nil
}

func eventError(event responsesEvent) error {
	if event.Error != nil && event.Error.Message != "" {
		return errors.New(event.Error.Message)
	}
	if event.Response != nil && event.Response.Error != nil && event.Response.Error.Message != "" {
		return errors.New(event.Response.Error.Message)
	}
	return fmt.Errorf("upstream Responses event %q", event.Type)
}

type streamTranslator struct {
	w             io.Writer
	fallbackModel string
	id            string
	model         string
	created       int64
	started       bool
	done          bool
	err           error

	textItems      map[string]bool
	refusalItems   map[string]bool
	reasoningItems map[string]bool
	toolIndexes    map[string]int
	toolStarted    map[int]bool
	toolNames      map[int]bool
	toolArgs       map[int]bool
	nextTool       int

	usage        *chatUsage
	finishReason string
}

func newStreamTranslator(w io.Writer, fallbackModel string) *streamTranslator {
	return &streamTranslator{
		w:              w,
		fallbackModel:  fallbackModel,
		textItems:      make(map[string]bool),
		refusalItems:   make(map[string]bool),
		reasoningItems: make(map[string]bool),
		toolIndexes:    make(map[string]int),
		toolStarted:    make(map[int]bool),
		toolNames:      make(map[int]bool),
		toolArgs:       make(map[int]bool),
	}
}

func (t *streamTranslator) handle(event responsesEvent) error {
	if t.err != nil {
		return t.err
	}

	switch event.Type {
	case "response.created", "response.in_progress":
		t.ensureStarted(event.Response)
	case "response.output_item.added":
		t.ensureStarted(event.Response)
		if event.Item != nil {
			switch event.Item.Type {
			case "function_call":
				t.startTool(event)
			case "message":
				t.textItems[eventItemKey(event)] = false
			case "reasoning":
				t.reasoningItems[eventItemKey(event)] = false
			}
		}
	case "response.output_text.delta":
		if event.Delta != "" {
			t.ensureStarted(event.Response)
			t.writeText(event.Delta)
			t.textItems[eventItemKey(event)] = true
		}
	case "response.refusal.delta":
		if event.Delta != "" {
			t.ensureStarted(event.Response)
			t.writeRefusal(event.Delta)
			t.refusalItems[eventItemKey(event)] = true
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if event.Delta != "" {
			t.ensureStarted(event.Response)
			t.writeReasoning(event.Delta)
			t.reasoningItems[eventItemKey(event)] = true
		}
	case "response.function_call_arguments.delta":
		t.ensureStarted(event.Response)
		t.writeToolArguments(event)
	case "response.output_item.done":
		if event.Item != nil {
			switch event.Item.Type {
			case "message":
				key := eventItemKey(event)
				if !t.textItems[key] {
					if text := messageText(event.Item); text != "" {
						t.ensureStarted(event.Response)
						t.writeText(text)
					}
				}
				if !t.refusalItems[key] {
					if refusal, ok := messageRefusal(event.Item); ok {
						t.ensureStarted(event.Response)
						t.writeRefusal(refusal)
					}
				}
				t.textItems[key] = true
				t.refusalItems[key] = true
			case "reasoning":
				key := eventItemKey(event)
				if !t.reasoningItems[key] {
					if text := reasoningText(event.Item); text != "" {
						t.ensureStarted(event.Response)
						t.writeReasoning(text)
					}
				}
				t.reasoningItems[key] = true
			case "function_call":
				t.finishTool(event)
			}
		}
	case "response.completed", "response.incomplete":
		t.complete(event.Response)
	case "response.failed", "error":
		t.writeError(eventError(event).Error())
	}
	return t.err
}

func (t *streamTranslator) finish() error {
	if t.done {
		return nil
	}
	return errors.New("upstream stream ended before response.completed")
}

func (t *streamTranslator) ensureStarted(response *responsesResponse) {
	if t.started {
		return
	}
	t.started = true
	t.created = time.Now().Unix()
	if response != nil {
		if response.ID != "" {
			t.id = response.ID
		}
		if response.Model != "" {
			t.model = response.Model
		}
		if response.CreatedAt > 0 {
			t.created = response.CreatedAt
		}
	}
	if t.id == "" {
		t.id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if t.model == "" {
		t.model = t.fallbackModel
	}

	empty := ""
	t.emitChunk(chatDelta{Role: "assistant", Content: &empty}, nil)
}

func (t *streamTranslator) writeText(text string) {
	t.emitChunk(chatDelta{Content: &text}, nil)
}

func (t *streamTranslator) writeReasoning(text string) {
	t.emitChunk(chatDelta{ReasoningContent: text}, nil)
}

func (t *streamTranslator) writeRefusal(text string) {
	t.emitChunk(chatDelta{Refusal: &text}, nil)
}

func (t *streamTranslator) startTool(event responsesEvent) {
	key := eventItemKey(event)
	index, exists := t.toolIndexes[key]
	if !exists {
		index = t.nextTool
		t.nextTool++
		t.toolIndexes[key] = index
	}
	if t.toolStarted[index] {
		return
	}
	t.toolStarted[index] = true

	name := ""
	id := event.ItemID
	if event.Item != nil {
		name = event.Item.Name
		if event.Item.CallID != "" {
			id = event.Item.CallID
		} else if event.Item.ID != "" {
			id = event.Item.ID
		}
	}
	empty := ""
	t.emitChunk(chatDelta{ToolCalls: []chatToolDelta{{
		Index:    index,
		ID:       id,
		Type:     "function",
		Function: &chatFunctionDelta{Name: name, Arguments: &empty},
	}}}, nil)
	t.toolNames[index] = name != ""
	t.toolArgs[index] = false
}

func (t *streamTranslator) writeToolArguments(event responsesEvent) {
	key := eventItemKey(event)
	index, exists := t.toolIndexes[key]
	if !exists {
		index = t.nextTool
		t.nextTool++
		t.toolIndexes[key] = index
		empty := ""
		t.emitChunk(chatDelta{ToolCalls: []chatToolDelta{{
			Index:    index,
			ID:       event.ItemID,
			Type:     "function",
			Function: &chatFunctionDelta{Arguments: &empty},
		}}}, nil)
		t.toolStarted[index] = true
		t.toolNames[index] = false
	}
	if event.Delta == "" {
		return
	}
	delta := event.Delta
	t.emitChunk(chatDelta{ToolCalls: []chatToolDelta{{
		Index:    index,
		Function: &chatFunctionDelta{Arguments: &delta},
	}}}, nil)
	t.toolArgs[index] = true
}

func (t *streamTranslator) finishTool(event responsesEvent) {
	key := eventItemKey(event)
	index, exists := t.toolIndexes[key]
	if !exists {
		t.startTool(event)
		index = t.toolIndexes[key]
	}
	if event.Item == nil {
		return
	}
	if !t.toolStarted[index] {
		t.startTool(event)
	}
	if event.Item.Name != "" && !t.toolNames[index] {
		empty := ""
		t.emitChunk(chatDelta{ToolCalls: []chatToolDelta{{
			Index:    index,
			Function: &chatFunctionDelta{Name: event.Item.Name, Arguments: &empty},
		}}}, nil)
	}
	if !t.toolArgs[index] && event.Item.Arguments != "" {
		arguments := event.Item.Arguments
		t.emitChunk(chatDelta{ToolCalls: []chatToolDelta{{
			Index:    index,
			Function: &chatFunctionDelta{Arguments: &arguments},
		}}}, nil)
		t.toolArgs[index] = true
	}
}

func (t *streamTranslator) complete(response *responsesResponse) {
	if t.done {
		return
	}
	t.ensureStarted(response)
	if response != nil {
		t.emitMissingOutput(response.Output)
		t.usage = chatUsageFromResponses(response.Usage)
		t.finishReason = finishReason(response, len(t.toolIndexes) > 0)
	}
	if t.finishReason == "" {
		t.finishReason = "stop"
	}

	finish := t.finishReason
	t.emitChunk(chatDelta{}, &finish)
	if t.usage != nil {
		t.emitUsage()
	}
	t.writeRaw("data: [DONE]\n\n")
	t.done = true
}

func (t *streamTranslator) emitMissingOutput(output []responseItem) {
	for index := range output {
		item := &output[index]
		key := itemKey(item, index)
		switch item.Type {
		case "message":
			if !t.textItems[key] {
				if text := messageText(item); text != "" {
					t.writeText(text)
				}
			}
			if !t.refusalItems[key] {
				if refusal, ok := messageRefusal(item); ok {
					t.writeRefusal(refusal)
				}
			}
			t.textItems[key] = true
			t.refusalItems[key] = true
		case "reasoning":
			if !t.reasoningItems[key] {
				if text := reasoningText(item); text != "" {
					t.writeReasoning(text)
				}
			}
			t.reasoningItems[key] = true
		case "function_call":
			event := responsesEvent{Item: item, OutputIndex: index}
			if _, exists := t.toolIndexes[key]; !exists {
				t.startTool(event)
			}
			toolIndex := t.toolIndexes[key]
			if !t.toolArgs[toolIndex] && item.Arguments != "" {
				arguments := item.Arguments
				t.emitChunk(chatDelta{ToolCalls: []chatToolDelta{{
					Index:    toolIndex,
					Function: &chatFunctionDelta{Arguments: &arguments},
				}}}, nil)
				t.toolArgs[toolIndex] = true
			}
		}
	}
}

func (t *streamTranslator) emitChunk(delta chatDelta, finish *string) {
	chunk := chatChunk{
		ID:      t.id,
		Object:  "chat.completion.chunk",
		Created: t.created,
		Model:   t.model,
		Choices: []chatChunkChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finish,
		}},
	}
	t.writeSSE(chunk)
}

func (t *streamTranslator) emitUsage() {
	chunk := chatChunk{
		ID:      t.id,
		Object:  "chat.completion.chunk",
		Created: t.created,
		Model:   t.model,
		Choices: []chatChunkChoice{},
		Usage:   t.usage,
	}
	t.writeSSE(chunk)
}

func (t *streamTranslator) writeError(message string) {
	if t.done || t.err != nil {
		return
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    "upstream_error",
		},
	}
	t.writeSSE(payload)
	t.writeRaw("data: [DONE]\n\n")
	t.done = true
}

func (t *streamTranslator) writeSSE(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		t.err = err
		return
	}
	t.writeRaw("data: " + string(data) + "\n\n")
}

func (t *streamTranslator) writeRaw(value string) {
	if t.err != nil {
		return
	}
	if _, err := io.WriteString(t.w, value); err != nil {
		t.err = err
	}
}

func itemKey(item *responseItem, index int) string {
	if item != nil && item.ID != "" {
		return item.ID
	}
	return "idx:" + strconv.Itoa(index)
}

func eventItemKey(event responsesEvent) string {
	if event.ItemID != "" {
		return event.ItemID
	}
	if event.Item != nil && event.Item.ID != "" {
		return event.Item.ID
	}
	return "idx:" + strconv.Itoa(event.OutputIndex)
}

// synthesizedStream turns a complete Responses response into a Chat
// Completions stream. It is used when the upstream returns plain JSON even
// though streaming was requested.
func synthesizedStream(final *responsesResponse, fallbackModel string) ([]byte, error) {
	response := chatCompletionFromResponses(final, fallbackModel)

	var buffer bytes.Buffer
	translator := newStreamTranslator(&buffer, fallbackModel)
	translator.id = response.ID
	translator.model = response.Model
	translator.created = response.Created
	translator.started = true

	empty := ""
	translator.emitChunk(chatDelta{Role: "assistant", Content: &empty}, nil)
	if choice := response.Choices[0]; choice.Message.Content != nil && *choice.Message.Content != "" {
		translator.writeText(*choice.Message.Content)
	}
	if choice := response.Choices[0]; choice.Message.ReasoningContent != "" {
		translator.writeReasoning(choice.Message.ReasoningContent)
	}
	if choice := response.Choices[0]; choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		translator.writeRefusal(*choice.Message.Refusal)
	}
	if choice := response.Choices[0]; len(choice.Message.ToolCalls) > 0 {
		for index, call := range choice.Message.ToolCalls {
			emptyArguments := ""
			translator.emitChunk(chatDelta{ToolCalls: []chatToolDelta{{
				Index:    index,
				ID:       call.ID,
				Type:     "function",
				Function: &chatFunctionDelta{Name: call.Function.Name, Arguments: &emptyArguments},
			}}}, nil)
			if call.Function.Arguments != "" {
				arguments := call.Function.Arguments
				translator.emitChunk(chatDelta{ToolCalls: []chatToolDelta{{
					Index:    index,
					Function: &chatFunctionDelta{Arguments: &arguments},
				}}}, nil)
			}
		}
	}

	finish := response.Choices[0].FinishReason
	translator.emitChunk(chatDelta{}, &finish)
	if response.Usage != nil {
		translator.usage = response.Usage
		translator.emitUsage()
	}
	translator.writeRaw("data: [DONE]\n\n")
	if translator.err != nil {
		return nil, translator.err
	}
	return buffer.Bytes(), nil
}
