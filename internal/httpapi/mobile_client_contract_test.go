package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/upload"
)

// TestMobileClientMultipartContract pins the exact request the mobile app
// (mobile/modules/nmcp-uploader, Android MediaUploader.kt and iOS UploadEngine.swift)
// sends: one part named "file", an ASCII filename fallback plus an RFC 8187
// filename*, a real media type, and a 69-byte "nmcp1-" content-derived key.
func TestMobileClientMultipartContract(t *testing.T) {
	const boundary = "nmcp-0123456789abcdef0123456789abcdef"
	const key = "nmcp1-9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	content := "\xff\xd8original-bytes\x00\r\n--not-a-boundary\r\n"
	body := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"IMG_____ (1).HEIC\"; filename*=UTF-8''IMG_%E5%86%99%E7%9C%9F%20%281%29.HEIC\r\n" +
		"Content-Type: image/heic\r\n" +
		"\r\n" + content + "\r\n" +
		"--" + boundary + "--\r\n"

	acceptor := &fakeUploadAcceptor{}
	var received []byte
	acceptor.accept = func(_ context.Context, request upload.Request) (upload.Outcome, error) {
		var err error
		received, err = io.ReadAll(request.Body)
		return upload.Outcome{Status: http.StatusCreated, Body: []byte(`{"media":{"id":"m"},"job":null}`)}, err
	}
	request := httptest.NewRequest(http.MethodPost, "/media", strings.NewReader(body))
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()

	NewHandler(Dependencies{Upload: acceptor}).ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if string(received) != content {
		t.Fatalf("original bytes were altered: %q", received)
	}
	seen := acceptor.requests[0]
	if seen.IdempotencyKey != key {
		t.Fatalf("key = %q", seen.IdempotencyKey)
	}
	if seen.Filename == nil || *seen.Filename != "IMG_写真 (1).HEIC" {
		t.Fatalf("filename = %v", seen.Filename)
	}
}
