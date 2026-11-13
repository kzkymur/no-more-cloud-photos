package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSupportedBackupManagerVersionIs255Or259(t *testing.T) {
	for version, want := range map[string]bool{
		"255": true, "255.4": true, "255.4-1ubuntu8": true, "255.4-1ubuntu8.6": true, "255~rc1": true,
		"254": false, "254.10-1": false, "256": false, "256.1-2": false, "257.9": false, "2550": false,
		"259": true, "259.5-0ubuntu3.4": true, "258": false, "260": false,
		"25": false, "": false, " 255": false, "255 ": false, "255.4-1ubuntu8\n": false, "v255": false,
	} {
		if got := supportedBackupManagerVersion(version); got != want {
			t.Errorf("supportedBackupManagerVersion(%q) = %t, want %t", version, got, want)
		}
	}
}

// The gate runs before any unit property is trusted: an unsupported manager
// fails closed with a sanitized reason and never reaches any allow-set.
func TestValidateLoadedBackupUnitRefusesOtherManagerVersion(t *testing.T) {
	saved := backupManagerVersion
	defer func() { backupManagerVersion = saved }()
	for _, version := range []string{"256.1-1", "254.13", "SECRET\nvalue"} {
		backupManagerVersion = func(context.Context) (string, error) { return version, nil }
		err := validateLoadedBackupUnit(context.Background(), "postgresql@17-main.service")
		if !errors.Is(err, errCutover) || !strings.Contains(err.Error(), "systemd manager version unsupported") || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("manager %q: %v", version, err)
		}
	}
	backupManagerVersion = func(context.Context) (string, error) { return "", backupUnitGuardError("manager version query") }
	if err := validateLoadedBackupUnit(context.Background(), "postgresql@17-main.service"); !errors.Is(err, errCutover) || !strings.Contains(err.Error(), "manager version query") {
		t.Fatalf("query failure: %v", err)
	}
}
