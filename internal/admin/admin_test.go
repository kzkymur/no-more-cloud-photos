package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestConfigValueValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		value string
		valid bool
	}{
		{"deleted_media_retention_days", "null", true},
		{"deleted_media_retention_days", "0", true},
		{"deleted_media_retention_days", "36500", true},
		{"deleted_media_retention_days", "36501", false},
		{"deleted_media_retention_days", "-1", false},
		{"superseded_rendition_retention_days", "null", true},
		{"superseded_rendition_retention_days", "0", true},
		{"superseded_rendition_retention_days", "2147483648", false},
		{"db_backup_interval_hours", "null", false},
		{"db_backup_interval_hours", "0", false},
		{"db_backup_interval_hours", "24", true},
		{"db_backup_retention_days", "30", true},
		{"db_backup_retention_days", "0", false},
		{"default_timezone", "Asia/Tokyo", true},
		{"default_timezone", "UTC", true},
		{"default_timezone", "America/New_York", true},
		{"default_timezone", "Local", false},
		{"default_timezone", "../secret", false},
		{"default_timezone", "Etc/Not_A_Zone", false},
		{"database_url", "secret-marker", false},
	} {
		t.Run(test.name+"/"+test.value, func(t *testing.T) {
			_, err := ConfigValue(test.name, test.value)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
		})
	}
}

func TestCLIParserRedactsMalformedFlags(t *testing.T) {
	for _, args := range [][]string{
		{"config", "show", "--json=secret-marker"},
		{"config", "show", "--secret-marker"},
		{"regenerate", "--batch-size=secret-marker", "--json"},
		{"jobs", "retry", "--additional-attempts=secret-marker"},
	} {
		var out, diagnostics bytes.Buffer
		code := Run(context.Background(), args, &out, &diagnostics)
		if code != 2 || strings.Contains(out.String()+diagnostics.String(), "secret-marker") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), diagnostics.String())
		}
	}
}

func TestCLIConfirmationAndValidationBeforeDatabase(t *testing.T) {
	id := "10000000-0000-4000-8000-000000000001"
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"regenerate", "--profile", "standard", "--all", "--json"}, 3},
		{[]string{"jobs", "retry", "--status", "failed", "--json"}, 3},
		{[]string{"repair", "--report", id, "--json"}, 3},
		{[]string{"maintenance", "resume", "--json"}, 3},
		{[]string{"cleanup", "--kind", "media", "--json"}, 3},
		{[]string{"regenerate", "--profile", "standard", "--all", "--media-id", id, "--json"}, 2},
		{[]string{"jobs", "retry", "--job-id", id, "--additional-attempts", "0", "--json"}, 2},
		{[]string{"cleanup", "--kind", "all", "--yes", "--json"}, 1},
		{[]string{"cleanup", "--kind", "backups", "--dry-run", "--json"}, 1},
		{[]string{"backup", "list", "--json"}, 1},
	} {
		var out, diagnostics bytes.Buffer
		if code := Run(context.Background(), test.args, &out, &diagnostics); code != test.code {
			t.Fatalf("args=%v code=%d want=%d stderr=%q", test.args, code, test.code, diagnostics.String())
		}
		decoder := json.NewDecoder(&out)
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if err := decoder.Decode(&value); err != io.EOF {
			t.Fatalf("stdout contained more than one value: %v", err)
		}
	}
}

func TestHumanConfigurationValuesNotPointerAddresses(t *testing.T) {
	zero, positive := 0, 30
	for _, value := range []*int{nil, &zero, &positive} {
		var out, diagnostics bytes.Buffer
		result := Configuration{DeletedMediaRetentionDays: value, DefaultTimezone: "Asia/Tokyo"}
		if code := finish(&out, &diagnostics, false, result, nil); code != 0 {
			t.Fatal(code)
		}
		if strings.Contains(out.String(), "0x") || !strings.Contains(out.String(), "deleted_media_retention_days") {
			t.Fatalf("unsafe human output %q", out.String())
		}
		var decoded Configuration
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if value == nil && decoded.DeletedMediaRetentionDays != nil || value != nil && (decoded.DeletedMediaRetentionDays == nil || *decoded.DeletedMediaRetentionDays != *value) {
			t.Fatalf("retention lost in human output %q", out.String())
		}
	}
}

func TestFinishRedactsOperationalErrors(t *testing.T) {
	var out, diagnostics bytes.Buffer
	if code := finish(&out, &diagnostics, true, nil, errors.New("postgres://user:secret-marker@host/db")); code != 1 {
		t.Fatal(code)
	}
	if strings.Contains(out.String()+diagnostics.String(), "secret-marker") {
		t.Fatal("secret leak")
	}
}
