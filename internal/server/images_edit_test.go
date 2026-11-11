package server_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/joe-p/model-proxy/internal/config"
)

func buildEditMultipart(t *testing.T, fields map[string]string, fileField, filename, contentType string, data []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatalf("write field %s: %v", name, err)
		}
	}
	if fileField != "" {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
			"name":     fileField,
			"filename": filename,
		}))
		header.Set("Content-Type", contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.Bytes(), writer.FormDataContentType()
}

func postEdit(t *testing.T, proxyURL, key, path string, body []byte, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

const editResponseBody = `{"created":1,"data":[{"b64_json":"aW1n"}],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46,"cost":0.05}}`

func TestImagesEditsForwardsMultipart(t *testing.T) {
	imageData := []byte("fake-png-bytes")
	var gotModel, gotPrompt, gotResponseFormat, gotBackground string
	var gotFile []byte
	var gotFilename, gotFilePath string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFilePath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("upstream method = %s", r.Method)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		gotModel = r.FormValue("model")
		gotPrompt = r.FormValue("prompt")
		gotResponseFormat = r.FormValue("response_format")
		gotBackground = r.FormValue("background")

		file, header, err := r.FormFile("image")
		if err != nil {
			t.Errorf("form file: %v", err)
		} else {
			defer file.Close()
			gotFilename = header.Filename
			gotFile, _ = io.ReadAll(file)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(editResponseBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		cfg.Models["image"] = config.ModelConfig{
			Alias:    "image",
			Provider: "phala",
			Upstream: "openai/gpt-image-1",
			ExtraBody: map[string]json.RawMessage{
				"background": json.RawMessage(`"transparent"`),
			},
		}
	})

	body, contentType := buildEditMultipart(t, map[string]string{
		"model":           "image",
		"prompt":          "make it blue",
		"n":               "1",
		"response_format": "b64_json",
		"background":      "opaque",
	}, "image", "source.png", "image/png", imageData)

	resp := postEdit(t, proxy.URL, "test-key", "/v1/images/edits", body, contentType)
	defer resp.Body.Close()
	gotBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, gotBody)
	}
	if gotFilePath != "/images/edits" {
		t.Fatalf("upstream path = %q", gotFilePath)
	}
	if gotModel != "openai/gpt-image-1" {
		t.Fatalf("upstream model = %q", gotModel)
	}
	if gotPrompt != "make it blue" {
		t.Fatalf("prompt = %q", gotPrompt)
	}
	if gotResponseFormat != "b64_json" {
		t.Fatalf("response_format = %q", gotResponseFormat)
	}
	// Model extra_body overrides the client field.
	if gotBackground != "transparent" {
		t.Fatalf("background = %q", gotBackground)
	}
	if gotFilename != "source.png" {
		t.Fatalf("filename = %q", gotFilename)
	}
	if !bytes.Equal(gotFile, imageData) {
		t.Fatalf("file changed: %q", gotFile)
	}
	if string(gotBody) != editResponseBody {
		t.Fatalf("body changed: %s", gotBody)
	}

	waitForLog(t, logs, `"cost_usd":0.05`)
	waitForLog(t, logs, `"prompt_tokens":12`)
	waitForLog(t, logs, `"completion_tokens":34`)
}

func TestImagesEditsTranslatesToInputReferences(t *testing.T) {
	imageData := []byte("fake-jpeg-bytes")
	var gotPath string
	var gotFields map[string]json.RawMessage

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("upstream content type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotFields); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(editResponseBody))
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyWithProvider(t, upstream.URL, "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.ImagesPath = "/images"
		providerCfg.ImagesEditFormat = config.ImagesEditInputReferences
		cfg.Providers["phala"] = providerCfg
		cfg.Models["image"] = config.ModelConfig{Alias: "image", Provider: "phala", Upstream: "google/gemini-2.5-flash-image"}
	})

	body, contentType := buildEditMultipart(t, map[string]string{
		"model":           "image",
		"prompt":          "make it blue",
		"n":               "2",
		"size":            "1024x1024",
		"response_format": "b64_json",
	}, "image", "source.jpg", "image/jpeg", imageData)

	resp := postEdit(t, proxy.URL, "test-key", "/v1/images/edits", body, contentType)
	defer resp.Body.Close()
	gotBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, gotBody)
	}
	if gotPath != "/images" {
		t.Fatalf("upstream path = %q", gotPath)
	}

	var model string
	if err := json.Unmarshal(gotFields["model"], &model); err != nil || model != "google/gemini-2.5-flash-image" {
		t.Fatalf("upstream model = %q (err %v)", model, err)
	}
	var prompt string
	if err := json.Unmarshal(gotFields["prompt"], &prompt); err != nil || prompt != "make it blue" {
		t.Fatalf("prompt = %q (err %v)", prompt, err)
	}
	var n int
	if err := json.Unmarshal(gotFields["n"], &n); err != nil || n != 2 {
		t.Fatalf("n = %d (err %v)", n, err)
	}
	if _, exists := gotFields["response_format"]; exists {
		t.Fatal("response_format must not be sent to the JSON image API")
	}

	var references []struct {
		Type     string `json:"type"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(gotFields["input_references"], &references); err != nil {
		t.Fatalf("decode input_references: %v", err)
	}
	if len(references) != 1 || references[0].Type != "image_url" {
		t.Fatalf("references = %+v", references)
	}
	const prefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(references[0].ImageURL.URL, prefix) {
		t.Fatalf("reference url = %q", references[0].ImageURL.URL)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(references[0].ImageURL.URL, prefix))
	if err != nil || !bytes.Equal(decoded, imageData) {
		t.Fatalf("reference data = %q (err %v)", decoded, err)
	}

	waitForLog(t, logs, `"cost_usd":0.05`)
	waitForLog(t, logs, `"upstream_model":"google/gemini-2.5-flash-image"`)
}

func TestImagesEditsRequiresModel(t *testing.T) {
	proxy, _ := newTestProxyWithProvider(t, "http://127.0.0.1:1", "openai-compatible", nil)
	body, contentType := buildEditMultipart(t, map[string]string{"prompt": "x"}, "", "", "", nil)
	resp := postEdit(t, proxy.URL, "test-key", "/v1/images/edits", body, contentType)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestImagesEditsRequiresMultipart(t *testing.T) {
	proxy, _ := newTestProxyWithProvider(t, "http://127.0.0.1:1", "openai-compatible", nil)
	resp := postEdit(t, proxy.URL, "test-key", "/v1/images/edits", []byte(`{"model":"image"}`), "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestImagesEditsRejectsMaskForJSONFormat(t *testing.T) {
	proxy, _ := newTestProxyWithProvider(t, "http://127.0.0.1:1", "openai-compatible", func(cfg *config.Config) {
		providerCfg := cfg.Providers["phala"]
		providerCfg.ImagesPath = "/images"
		providerCfg.ImagesEditFormat = config.ImagesEditInputReferences
		cfg.Providers["phala"] = providerCfg
		cfg.Models["image"] = config.ModelConfig{Alias: "image", Provider: "phala", Upstream: "img"}
	})

	body, contentType := buildEditMultipart(t, map[string]string{
		"model":  "image",
		"prompt": "x",
	}, "mask", "mask.png", "image/png", []byte("mask"))
	resp := postEdit(t, proxy.URL, "test-key", "/v1/images/edits", body, contentType)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
