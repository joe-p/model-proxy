package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/joe-p/model-proxy/internal/auth"
	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/provider"
)

type imageEditFile struct {
	field       string
	filename    string
	contentType string
	data        []byte
}

type imageEditForm struct {
	fields url.Values
	files  []imageEditFile
}

// handleImagesEdits proxies image edit requests. Multipart providers receive
// the form with only the model field rewritten. Providers configured with
// images_edit_format "input_references" get a JSON body instead, with uploaded
// files encoded as base64 data URLs, because APIs such as OpenRouter's images
// endpoint do not accept multipart edits.
func (s *Server) handleImagesEdits(w http.ResponseWriter, r *http.Request) {
	lr := logRecordFrom(r.Context())
	if r.Method != http.MethodPost {
		writeAPIError(w, lr, errMethodNotAllowed(http.MethodPost))
		return
	}

	st := stateFrom(r.Context())
	cfg := st.cfg

	key, ok := auth.Authenticate(r, cfg.Auth.Enabled, cfg.Keys)
	if !ok {
		writeAPIError(w, lr, errInvalidAPIKey())
		return
	}
	logKey(lr, cfg, key)

	body, err := readRequestBody(w, r, imageBodyLimit(cfg))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(w, lr, errRequestBodyTooLarge())
			return
		}
		if r.Context().Err() != nil {
			lr.clientCanceled = true
			return
		}
		writeAPIError(w, lr, errInvalidRequest("Unable to read request body"))
		return
	}

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeAPIError(w, lr, errInvalidRequest("Request Content-Type must be multipart/form-data"))
		return
	}
	form, err := parseImageEditMultipart(body, params["boundary"])
	body = nil
	if err != nil {
		writeAPIError(w, lr, errInvalidRequest("Unable to parse multipart request body"))
		return
	}

	requestedModel := strings.TrimSpace(form.fields.Get("model"))
	if requestedModel == "" {
		writeAPIError(w, lr, errInvalidRequest("Request field \"model\" is required"))
		return
	}

	model, resolveErr := resolveModel(cfg, key, requestedModel)
	if resolveErr != nil {
		writeAPIError(w, lr, resolveErr)
		return
	}

	prov, exists := st.providers[model.Provider]
	if !exists {
		lr.errorDetail = fmt.Sprintf("provider %q is not available", model.Provider)
		writeAPIError(w, lr, errInternal())
		return
	}

	lr.alias = model.Alias
	lr.provider = model.Provider
	lr.billingMode = cfg.Providers[model.Provider].BillingMode
	lr.upstream = model.Upstream
	lr.passthrough = model.Passthrough

	format := cfg.Providers[model.Provider].ImagesEditFormat
	if format == "" {
		format = config.ImagesEditMultipart
	}

	var resp *http.Response
	switch format {
	case config.ImagesEditInputReferences:
		resp, err = sendJSONImageEdit(r, lr, prov, model, form)
	default:
		resp, err = sendMultipartImageEdit(r, lr, prov, model, form)
	}
	if err != nil {
		if r.Context().Err() != nil {
			lr.clientCanceled = true
			return
		}
		var requestErr *apiError
		if errors.As(err, &requestErr) {
			writeAPIError(w, lr, requestErr)
			return
		}
		lr.errorDetail = err.Error()
		var providerErr *provider.Error
		if errors.As(err, &providerErr) && providerErr.Kind == provider.Timeout {
			writeAPIError(w, lr, errUpstreamTimeout())
		} else {
			writeAPIError(w, lr, errUpstreamUnavailable())
		}
		return
	}
	defer resp.Body.Close()
	s.writeImageResponse(w, r, st, lr, model, resp)
}

func parseImageEditMultipart(body []byte, boundary string) (*imageEditForm, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	form := &imageEditForm{fields: make(url.Values)}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return form, nil
		}
		if err != nil {
			return nil, err
		}

		name := part.FormName()
		if name == "" {
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			return nil, err
		}

		if filename := part.FileName(); filename != "" {
			form.files = append(form.files, imageEditFile{
				field:       name,
				filename:    filename,
				contentType: part.Header.Get("Content-Type"),
				data:        data,
			})
			continue
		}
		form.fields.Add(name, string(data))
	}
}

func sendMultipartImageEdit(r *http.Request, lr *requestLog, prov provider.Provider, model config.ModelConfig, form *imageEditForm) (*http.Response, error) {
	editor, ok := prov.(provider.ImageEditor)
	if !ok {
		return nil, errInvalidRequest(fmt.Sprintf("Provider %q does not support image edits", model.Provider))
	}

	fields := make(map[string]json.RawMessage, len(form.fields)+len(model.ExtraBody))
	for name, values := range form.fields {
		if len(values) == 0 {
			continue
		}
		encoded, err := json.Marshal(values[0])
		if err != nil {
			return nil, err
		}
		fields[name] = encoded
	}
	upstreamModel, err := json.Marshal(model.Upstream)
	if err != nil {
		return nil, err
	}
	fields["model"] = upstreamModel
	effective, err := config.MergeExtraBody(fields, model.ExtraBody)
	if err != nil {
		return nil, err
	}

	body, contentType, err := encodeMultipartImageEdit(effective, form.files)
	if err != nil {
		return nil, err
	}
	return editor.ImagesEdit(r.Context(), provider.Request{
		UpstreamModel: model.Upstream,
		Body:          body,
		ContentType:   contentType,
		Accept:        r.Header.Get("Accept"),
		RequestID:     lr.requestID,
		Query:         r.URL.RawQuery,
	})
}

func sendJSONImageEdit(r *http.Request, lr *requestLog, prov provider.Provider, model config.ModelConfig, form *imageEditForm) (*http.Response, error) {
	images, ok := prov.(provider.ImagesProvider)
	if !ok {
		return nil, errInvalidRequest(fmt.Sprintf("Provider %q does not support image edits", model.Provider))
	}

	fields := make(map[string]json.RawMessage, len(form.fields)+2)
	for name, values := range form.fields {
		if len(values) == 0 {
			continue
		}
		switch name {
		case "model", "image", "image[]", "mask", "response_format":
			continue
		}
		fields[name] = imageEditFormValue(name, values[0])
	}

	references := make([]json.RawMessage, 0, len(form.files)+2)
	for _, file := range form.files {
		if file.field == "mask" {
			return nil, errInvalidRequest(fmt.Sprintf("Provider %q does not support image edit masks", model.Provider))
		}
		reference, err := imageReferenceDataURL(file.contentType, file.data)
		if err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	for _, name := range []string{"image", "image[]"} {
		for _, value := range form.fields[name] {
			reference, err := imageReferenceURL(value)
			if err != nil {
				return nil, err
			}
			references = append(references, reference)
		}
	}
	if len(references) == 0 {
		return nil, errInvalidRequest("Request field \"image\" is required")
	}
	encodedReferences, err := json.Marshal(references)
	if err != nil {
		return nil, err
	}
	fields["input_references"] = encodedReferences

	upstreamModel, err := json.Marshal(model.Upstream)
	if err != nil {
		return nil, err
	}
	fields["model"] = upstreamModel

	effective, err := config.MergeExtraBody(fields, model.ExtraBody)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(effective)
	if err != nil {
		return nil, err
	}

	return images.Images(r.Context(), provider.Request{
		UpstreamModel: model.Upstream,
		Body:          body,
		Accept:        r.Header.Get("Accept"),
		RequestID:     lr.requestID,
		Query:         r.URL.RawQuery,
	})
}

// imageEditFormValue converts multipart text fields into JSON values. Numeric
// and object fields must not be sent as strings to JSON image APIs.
func imageEditFormValue(name, value string) json.RawMessage {
	switch name {
	case "n", "seed", "frame_count", "frame_duration_ms", "output_compression", "provider", "input_references":
		if json.Valid([]byte(value)) {
			return json.RawMessage(value)
		}
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

func imageReferenceDataURL(contentType string, data []byte) (json.RawMessage, error) {
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	encoded := "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)
	return imageReferenceURL(encoded)
}

// imageReference is one OpenRouter-style input reference.
type imageReference struct {
	Type     string `json:"type"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

func imageReferenceURL(value string) (json.RawMessage, error) {
	if !strings.HasPrefix(value, "data:") && !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return nil, errInvalidRequest("Image edit references must be data URLs or HTTP(S) URLs")
	}
	reference := imageReference{Type: "image_url"}
	reference.ImageURL.URL = value
	return json.Marshal(reference)
}

// encodeMultipartImageEdit rebuilds the edit form with the resolved fields and
// the original file parts and their metadata.
func encodeMultipartImageEdit(fields map[string]json.RawMessage, files []imageEditFile) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for name, raw := range fields {
		value := string(raw)
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			value = text
		}
		if err := writer.WriteField(name, value); err != nil {
			return nil, "", err
		}
	}
	for _, file := range files {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
			"name":     file.field,
			"filename": file.filename,
		}))
		if file.contentType != "" {
			header.Set("Content-Type", file.contentType)
		}
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(file.data); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}
