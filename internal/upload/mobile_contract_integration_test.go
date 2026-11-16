//go:build linux

package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// TestMobileClientResponseContractIntegration pins the response shapes the
// mobile uploaders (mobile/modules/nmcp-uploader, MediaUploader.kt classify and
// UploadEngine.swift) depend on, through the real repository:
//   - 201 with media.id
//   - same key + same request: Replayed (the HTTP layer adds Idempotency-Replayed: true)
//   - new key, same bytes: 409 error.code "duplicate_media" with error.details.existing_media_id
//   - same key, different filename: 409 error.code "idempotency_conflict" (the clients
//     put the filename into the key so they never produce this)
func TestMobileClientResponseContractIntegration(t *testing.T) {
	pool := uploadIntegrationPool(t, nil)
	service, _, _ := integrationService(t, pool, "", nil)
	ctx := context.Background()
	request := func(key, requestID, filename, content string) Request {
		name := filename
		return Request{Body: bytes.NewBufferString(content), Filename: &name, IdempotencyKey: key, RequestID: requestID}
	}
	type errorBody struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}

	created, err := service.Accept(ctx, request("nmcp1-a", "r1", "IMG_0001.HEIC", "original-bytes"))
	if err != nil || created.Status != 201 || created.Replayed {
		t.Fatalf("first upload = %+v, %v", created, err)
	}
	var success struct {
		Media struct {
			ID string `json:"id"`
		} `json:"media"`
	}
	if err := json.Unmarshal(created.Body, &success); err != nil || success.Media.ID == "" {
		t.Fatalf("201 body has no media.id: %s (%v)", created.Body, err)
	}

	replay, err := service.Accept(ctx, request("nmcp1-a", "r2", "IMG_0001.HEIC", "original-bytes"))
	if err != nil || replay.Status != 201 || !replay.Replayed {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	// The replayed body is the stored JSON (re-serialized), so compare the field the clients read.
	var replayed struct {
		Media struct {
			ID string `json:"id"`
		} `json:"media"`
	}
	if err := json.Unmarshal(replay.Body, &replayed); err != nil || replayed.Media.ID != success.Media.ID {
		t.Fatalf("replay media.id = %q, want %q (%v)", replayed.Media.ID, success.Media.ID, err)
	}

	duplicate, err := service.Accept(ctx, request("nmcp1-b", "r3", "IMG_0001 copy.HEIC", "original-bytes"))
	if err != nil || duplicate.Status != 409 {
		t.Fatalf("duplicate = %+v, %v", duplicate, err)
	}
	var dup errorBody
	if err := json.Unmarshal(duplicate.Body, &dup); err != nil || dup.Error.Code != "duplicate_media" ||
		dup.Error.Details["existing_media_id"] != success.Media.ID {
		t.Fatalf("duplicate body = %s (%v)", duplicate.Body, err)
	}

	conflict, err := service.Accept(ctx, request("nmcp1-a", "r4", "renamed.HEIC", "original-bytes"))
	if err != nil || conflict.Status != 409 || conflict.Replayed {
		t.Fatalf("conflict = %+v, %v", conflict, err)
	}
	var conf errorBody
	if err := json.Unmarshal(conflict.Body, &conf); err != nil || conf.Error.Code != "idempotency_conflict" {
		t.Fatalf("conflict body = %s (%v)", conflict.Body, err)
	}
}
