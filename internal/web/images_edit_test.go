package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestImageEditsValidation(t *testing.T) {
	t.Run("method", func(t *testing.T) {
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, httptest.NewRequest(http.MethodGet, "/v1/images/edits", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status=%d want %d", w.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("prompt", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("image", "image.png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte("not reached without a prompt"))
		_ = writer.Close()
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("image", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("prompt", "make it blue")
		_ = writer.Close()
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d want %d", w.Code, http.StatusBadRequest)
		}
	})
}
