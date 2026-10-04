package readapi

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestContractJSONSchemas(t *testing.T) {
	now := time.Date(2026, time.October, 4, 12, 34, 56, 123456789, time.UTC)
	profile := ProfileRef{ID: testIDs[2], Key: "standard", Version: 1}
	rendition := Rendition{
		ID: testIDs[1], MediaID: testIDs[0], JobTargetID: testIDs[3], Profile: profile,
		MIMEType: "image/avif", SHA256: sixtyFourZeroes, FileURL: "https://files.example/renditions/output.avif", CreatedAt: now,
	}
	media := MediaSummary{ID: testIDs[0], MIMEType: "image/jpeg", TakenAtSource: TakenAtUnknown, CreatedAt: now}
	detail := NewMediaDetail()
	detail.ID, detail.MIMEType, detail.TakenAtSource, detail.CreatedAt = testIDs[0], "image/jpeg", TakenAtUnknown, now
	original := Original{ID: testIDs[4], MediaID: testIDs[0], MIMEType: "image/jpeg", SHA256: sixtyFourZeroes, FileURL: "https://files.example/originals/original.jpg", CreatedAt: now}
	target := JobTarget{ID: testIDs[3], Profile: profile, Status: TargetPending, UpdatedAt: now}
	job := NewJob()
	job.ID, job.Type, job.Status, job.MediaID = testIDs[5], JobTransform, JobQueued, testIDs[0]
	job.AvailableAt, job.CreatedAt, job.UpdatedAt = now, now, now
	job.Targets = append(job.Targets, target)
	readProfile := NewProfile()
	readProfile.ID, readProfile.Key, readProfile.Version, readProfile.Status = testIDs[2], "standard", 1, ProfileActive
	readProfile.ParametersSchemaVersion, readProfile.Processor = 1, "nmcp-media"
	readProfile.Parameters, readProfile.CreatedAt = json.RawMessage(`{"quality":9007199254740993}`), now

	tests := []struct {
		name  string
		value any
		keys  []string
	}{
		{"profile ref", profile, []string{"id", "key", "version"}},
		{"rendition", rendition, []string{"created_at", "duration_ms", "file_url", "height", "id", "job_target_id", "media_id", "mime_type", "profile", "sha256", "size_bytes", "width"}},
		{"media summary", media, []string{"created_at", "deleted_at", "duration_ms", "height", "id", "mime_type", "original_filename", "purge_after", "rendition", "size_bytes", "taken_at", "taken_at_source", "taken_at_timezone", "width"}},
		{"media detail", detail, []string{"created_at", "current_renditions", "deleted_at", "duration_ms", "height", "id", "jobs", "mime_type", "original_filename", "purge_after", "size_bytes", "taken_at", "taken_at_source", "taken_at_timezone", "width"}},
		{"original", original, []string{"created_at", "file_url", "id", "media_id", "mime_type", "original_filename", "sha256", "size_bytes"}},
		{"job", job, []string{"attempts", "available_at", "cancel_reason", "cancelled_at", "created_at", "error", "finished_at", "id", "max_attempts", "media_id", "original_id", "started_at", "status", "targets", "type", "updated_at"}},
		{"job target", target, []string{"attempts", "error", "id", "profile", "rendition_id", "status", "updated_at"}},
		{"profile", readProfile, []string{"activated_at", "created_at", "id", "input_mime_types", "key", "parameters", "parameters_schema_version", "processor", "retired_at", "status", "version"}},
		{"media page", NewMediaPage(nil, nil), []string{"items", "next_cursor"}},
		{"job page", NewJobPage(nil, nil), []string{"items", "next_cursor"}},
		{"profile page", NewProfilePage(nil), []string{"items"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatal(err)
			}
			actual := make([]string, 0, len(object))
			for key := range object {
				actual = append(actual, key)
			}
			slices.Sort(actual)
			if !slices.Equal(actual, test.keys) {
				t.Fatalf("JSON keys = %v, want %v; body=%s", actual, test.keys, encoded)
			}
		})
	}

	encoded, err := json.Marshal(readProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) || !containsJSONNumberExactly(encoded, `"quality":9007199254740993`) {
		t.Fatalf("profile parameters lost raw numeric representation: %s", encoded)
	}
}

func TestNullableFieldsAndArraysHaveExactJSONKinds(t *testing.T) {
	now := time.Date(2026, time.October, 4, 1, 2, 3, 4, time.UTC)
	values := []any{
		MediaSummary{CreatedAt: now},
		NewMediaDetail(),
		Original{CreatedAt: now},
		NewJob(),
		JobTarget{UpdatedAt: now},
		NewProfile(),
		NewMediaPage(nil, nil),
		NewJobPage([]Job{{}}, nil),
		NewProfilePage([]Profile{{}}),
	}
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if hasJSONNullArray(encoded, "current_renditions") || hasJSONNullArray(encoded, "jobs") ||
			hasJSONNullArray(encoded, "targets") || hasJSONNullArray(encoded, "input_mime_types") ||
			hasJSONNullArray(encoded, "items") {
			t.Fatalf("array encoded as null: %s", encoded)
		}
	}

	mediaJSON, _ := json.Marshal(MediaSummary{CreatedAt: now})
	assertJSONNulls(t, mediaJSON, "original_filename", "width", "height", "duration_ms", "taken_at", "taken_at_timezone", "deleted_at", "purge_after", "rendition")
	jobJSON, _ := json.Marshal(NewJob())
	assertJSONNulls(t, jobJSON, "original_id", "started_at", "finished_at", "error", "cancelled_at", "cancel_reason")
	targetJSON, _ := json.Marshal(JobTarget{})
	assertJSONNulls(t, targetJSON, "error", "rendition_id")
}

func TestInitializeArraysRepairsScannedModels(t *testing.T) {
	detail := MediaDetail{Jobs: []Job{{}}}
	detail.InitializeArrays()
	if detail.CurrentRenditions == nil || detail.Jobs[0].Targets == nil {
		t.Fatalf("media detail arrays were not initialized: %#v", detail)
	}
	profile := Profile{}
	profile.InitializeArrays()
	if profile.InputMIMETypes == nil {
		t.Fatal("profile input MIME types were not initialized")
	}
}

func TestContractUsesTimeTimeAndHasNoOmitEmpty(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeFor[Rendition](), reflect.TypeFor[MediaSummary](), reflect.TypeFor[MediaDetail](),
		reflect.TypeFor[Original](), reflect.TypeFor[Job](), reflect.TypeFor[JobTarget](), reflect.TypeFor[Profile](),
	}
	timeType := reflect.TypeFor[time.Time]()
	foundTime := false
	for _, model := range types {
		for index := range model.NumField() {
			field := model.Field(index)
			if field.Type == timeType || field.Type == reflect.PointerTo(timeType) {
				foundTime = true
			}
			if tag := field.Tag.Get("json"); tag == "" || tag == "-" || containsJSONNumberExactly([]byte(tag), "omitempty") {
				t.Fatalf("%s.%s has invalid JSON tag %q", model.Name(), field.Name, tag)
			}
		}
	}
	if !foundTime {
		t.Fatal("contract has no time.Time fields")
	}
}

func TestQueryEnumsAndRequests(t *testing.T) {
	media := NewMediaListRequest()
	if media.Profile != "standard" || media.Deleted != DeletedExclude || media.Limit != 50 || media.Validate() != nil {
		t.Fatalf("default media request = %#v, err=%v", media, media.Validate())
	}
	jobs := NewJobListRequest()
	jobs.MediaID = testIDs[0]
	if jobs.Validate() != nil {
		t.Fatalf("valid jobs request: %v", jobs.Validate())
	}
	profiles := ProfileListRequest{Status: ProfileRetired}
	if profiles.Validate() != nil {
		t.Fatalf("valid profile request: %v", profiles.Validate())
	}

	invalid := []struct {
		name  string
		value interface{ Validate() error }
		field string
	}{
		{"profile key", MediaListRequest{Profile: "UPPER", Deleted: DeletedExclude, Limit: 1}, "profile"},
		{"deleted", MediaListRequest{Profile: "standard", Deleted: "all", Limit: 1}, "deleted"},
		{"media limit", MediaListRequest{Profile: "standard", Deleted: DeletedExclude, Limit: 201}, "limit"},
		{"job status", JobListRequest{Status: "done", Limit: 1}, "status"},
		{"job media ID", JobListRequest{MediaID: "not-an-id", Limit: 1}, "media_id"},
		{"job limit", JobListRequest{Limit: 0}, "limit"},
		{"profile status", ProfileListRequest{Status: "disabled"}, "status"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			var semantic *SemanticError
			if err := test.value.Validate(); !errors.As(err, &semantic) || semantic.Code() != CodeInvalidRequest {
				t.Fatalf("Validate() error = %#v", err)
			}
			fields := semantic.Response("request").Error.Details["fields"].(map[string]string)
			if fields[test.field] == "" {
				t.Fatalf("fields = %#v, missing %q", fields, test.field)
			}
		})
	}

	if _, err := ParseDeletedFilter("include"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJobStatus("failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseProfileStatus("active"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDeletedFilter("INCLUDE"); err == nil {
		t.Fatal("uppercase deleted filter accepted")
	}
}

func assertJSONNulls(t *testing.T, encoded []byte, fields ...string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		if string(object[field]) != "null" {
			t.Errorf("%s = %s, want null; body=%s", field, object[field], encoded)
		}
	}
}

func hasJSONNullArray(encoded []byte, field string) bool {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &object)
	value, exists := object[field]
	return exists && string(value) == "null"
}

func containsJSONNumberExactly(encoded []byte, fragment string) bool {
	for index := 0; index+len(fragment) <= len(encoded); index++ {
		if string(encoded[index:index+len(fragment)]) == fragment {
			return true
		}
	}
	return false
}

const sixtyFourZeroes = "0000000000000000000000000000000000000000000000000000000000000000"

var testIDs = [...]string{
	"00000000-0000-4000-8000-000000000001",
	"00000000-0000-4000-9000-000000000002",
	"00000000-0000-4000-a000-000000000003",
	"00000000-0000-4000-b000-000000000004",
	"10000000-0000-4000-8000-000000000005",
	"20000000-0000-4000-8000-000000000006",
}
