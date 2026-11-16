package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/upload"
)

// mobileContractVectors is the wire format the mobile clients produce. The same
// file is asserted by the Kotlin (MultipartFormatTest.kt) and Swift
// (tests/swift/main.swift) unit tests, so a client that drifts from what this
// handler accepts fails its own test.
const mobileContractVectors = "../../mobile/modules/nmcp-uploader/testdata/contract-vectors.json"

type mobileContractCase struct {
	Platform      string `json:"platform"`
	ItemID        string `json:"itemId"`
	Name          string `json:"name"`
	ContentLatin1 string `json:"contentLatin1"`
	Boundary      string `json:"boundary"`
	Expected      struct {
		IdempotencyKey string `json:"idempotencyKey"`
		Head           string `json:"head"`
		Tail           string `json:"tail"`
		ServerFilename string `json:"serverFilename"`
	} `json:"expected"`
}

func latin1Bytes(t *testing.T, s string) []byte {
	t.Helper()
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xff {
			t.Fatalf("contentLatin1 contains %U", r)
		}
		out = append(out, byte(r))
	}
	return out
}

func TestMobileClientMultipartContract(t *testing.T) {
	raw, err := os.ReadFile(mobileContractVectors)
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []mobileContractCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) == 0 {
		t.Fatal("no contract vectors")
	}
	for _, c := range vectors.Cases {
		t.Run(c.Platform+"/"+c.Name, func(t *testing.T) {
			content := latin1Bytes(t, c.ContentLatin1)
			if err := upload.ValidateIdempotencyKey(c.Expected.IdempotencyKey); err != nil {
				t.Fatalf("client key rejected: %v", err)
			}
			if len(c.Expected.IdempotencyKey) != 70 {
				t.Fatalf("key length = %d, want 70", len(c.Expected.IdempotencyKey))
			}

			acceptor := &fakeUploadAcceptor{}
			var received []byte
			acceptor.accept = func(_ context.Context, request upload.Request) (upload.Outcome, error) {
				var readErr error
				received, readErr = io.ReadAll(request.Body)
				return upload.Outcome{Status: http.StatusCreated, Body: []byte(`{"media":{"id":"m"},"job":null}`)}, readErr
			}
			body := c.Expected.Head + string(content) + c.Expected.Tail
			request := httptest.NewRequest(http.MethodPost, "/media", strings.NewReader(body))
			request.Header.Set("Content-Type", "multipart/form-data; boundary="+c.Boundary)
			request.Header.Set("Accept", "application/json")
			request.Header.Set("Idempotency-Key", c.Expected.IdempotencyKey)
			response := httptest.NewRecorder()

			NewHandler(Dependencies{Upload: acceptor}).ServeHTTP(response, request)

			if response.Code != http.StatusCreated {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			if string(received) != string(content) {
				t.Fatalf("original bytes were altered: %q", received)
			}
			seen := acceptor.requests[0]
			if seen.IdempotencyKey != c.Expected.IdempotencyKey {
				t.Fatalf("key = %q", seen.IdempotencyKey)
			}
			if seen.Filename == nil || *seen.Filename != c.Expected.ServerFilename {
				t.Fatalf("filename = %v, want %q", seen.Filename, c.Expected.ServerFilename)
			}
		})
	}
}
