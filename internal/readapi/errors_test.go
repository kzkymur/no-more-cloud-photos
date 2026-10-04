package readapi

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestSemanticErrorsMapToPublicHTTPContract(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.4:5432: secret database failure")
	tests := []struct {
		name   string
		err    *SemanticError
		kind   ErrorKind
		status int
		code   ErrorCode
	}{
		{"media not found", NewMediaNotFound(), KindNotFound, 404, CodeMediaNotFound},
		{"original not found", NewOriginalNotFound(), KindNotFound, 404, CodeOriginalNotFound},
		{"rendition not found", NewRenditionNotFound(), KindNotFound, 404, CodeRenditionNotFound},
		{"job not found", NewJobNotFound(), KindNotFound, 404, CodeJobNotFound},
		{"rendition not ready", NewRenditionNotReady(), KindRenditionNotReady, 409, CodeRenditionNotReady},
		{"invalid profile", NewInvalidProfile("missing"), KindInvalidProfile, 400, CodeInvalidProfile},
		{"invariant", NewInvariantError(cause), KindInvariant, 500, CodeInternalError},
		{"database unavailable", NewDatabaseUnavailableError(cause), KindDatabaseUnavailable, 503, CodeUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.err.HTTPStatus() != test.status || test.err.Code() != test.code || test.err.Kind() != test.kind || !IsKind(test.err, test.kind) {
				t.Fatalf("semantic error = status %d code %q kind %d", test.err.HTTPStatus(), test.err.Code(), test.err.Kind())
			}
			response := test.err.Response("request-123")
			if response.Error.Code != test.code || response.Error.RequestID != "request-123" || response.Error.Details == nil {
				t.Fatalf("response = %#v", response)
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"10.0.0.4", "5432", "secret", "cause", "status", "kind"} {
				if containsJSONNumberExactly(encoded, forbidden) {
					t.Fatalf("internal field leaked in %s", encoded)
				}
			}
		})
	}

	if !errors.Is(NewInvariantError(cause), cause) || !errors.Is(NewDatabaseUnavailableError(cause), cause) {
		t.Fatal("private causes are not available to trusted error inspection")
	}
}

func TestErrorResponseExactJSONAndDefensiveDetails(t *testing.T) {
	fields := map[string]string{"limit": "out_of_range"}
	semantic := NewInvalidRequest(fields)
	fields["limit"] = "changed"
	response := semantic.Response("01JREQUEST")
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"error":{"code":"invalid_request","message":"request is invalid","request_id":"01JREQUEST","details":{"fields":{"limit":"out_of_range"}}}}`
	if string(encoded) != want {
		t.Fatalf("response JSON = %s, want %s", encoded, want)
	}

	empty, err := json.Marshal(NewMediaNotFound().Response("request"))
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"error":{"code":"media_not_found","message":"media was not found","request_id":"request","details":{}}}` {
		t.Fatalf("empty details response = %s", empty)
	}
}
