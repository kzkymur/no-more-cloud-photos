package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadAPIDefaults(t *testing.T) {
	cfg, err := LoadAPIFrom(env(map[string]string{
		databaseURLEnv:   "postgres://user:password@db/photos",
		storageRootEnv:   "/srv/nmcp/media",
		fileBaseURLEnv:   "https://files.example.test",
		cursorHMACKeyEnv: strings.Repeat("k", 32),
	}))
	if err != nil {
		t.Fatalf("LoadAPIFrom() error = %v", err)
	}
	if cfg.Addr != DefaultAPIAddr || cfg.LogLevel != DefaultLogLevel || cfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Fatalf("defaults = addr %q, level %q, timeout %s", cfg.Addr, cfg.LogLevel, cfg.ShutdownTimeout)
	}
}

func TestLoadAPIValid(t *testing.T) {
	cfg, err := LoadAPIFrom(env(map[string]string{
		databaseURLEnv:     "postgres://db-secret",
		storageRootEnv:     "/var/lib/nmcp",
		fileBaseURLEnv:     "https://files.example.test/media",
		cursorHMACKeyEnv:   strings.Repeat("\xC3\xA9", 16),
		apiAddrEnv:         "[::1]:9443",
		logLevelEnv:        "DEBUG",
		shutdownTimeoutEnv: "45s",
	}))
	if err != nil {
		t.Fatalf("LoadAPIFrom() error = %v", err)
	}
	if cfg.DatabaseURL != "postgres://db-secret" || cfg.StorageRoot != "/var/lib/nmcp" || cfg.FileBaseURL != "https://files.example.test/media" {
		t.Fatalf("LoadAPIFrom() returned unexpected required values: %#v", cfg)
	}
	if cfg.Addr != "[::1]:9443" || cfg.LogLevel != "debug" || cfg.ShutdownTimeout != 45*time.Second {
		t.Fatalf("LoadAPIFrom() returned unexpected optional values: %#v", cfg)
	}
	if len(cfg.CursorHMACKey) != 32 {
		t.Fatalf("cursor key length = %d, want 32 bytes", len(cfg.CursorHMACKey))
	}
}

func TestLoadWorkerDefaultsAndValid(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want WorkerConfig
	}{
		{
			name: "defaults",
			vars: map[string]string{databaseURLEnv: "postgres://localhost/photos", storageRootEnv: "/data"},
			want: WorkerConfig{DatabaseURL: "postgres://localhost/photos", StorageRoot: "/data", LogLevel: "info", ShutdownTimeout: 30 * time.Second},
		},
		{
			name: "valid overrides",
			vars: map[string]string{databaseURLEnv: "postgres://localhost/photos", storageRootEnv: "/data", logLevelEnv: "warn", shutdownTimeoutEnv: "2m"},
			want: WorkerConfig{DatabaseURL: "postgres://localhost/photos", StorageRoot: "/data", LogLevel: "warn", ShutdownTimeout: 2 * time.Minute},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadWorkerFrom(env(tt.vars))
			if err != nil {
				t.Fatalf("LoadWorkerFrom() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("LoadWorkerFrom() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLoadAdminDefaultsAndValid(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want AdminConfig
	}{
		{
			name: "defaults",
			vars: map[string]string{databaseURLEnv: "postgres://localhost/photos"},
			want: AdminConfig{DatabaseURL: "postgres://localhost/photos", LogLevel: "info", ShutdownTimeout: 30 * time.Second},
		},
		{
			name: "valid overrides",
			vars: map[string]string{databaseURLEnv: "postgres://localhost/photos", logLevelEnv: "error", shutdownTimeoutEnv: "1s"},
			want: AdminConfig{DatabaseURL: "postgres://localhost/photos", LogLevel: "error", ShutdownTimeout: time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadAdminFrom(env(tt.vars))
			if err != nil {
				t.Fatalf("LoadAdminFrom() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("LoadAdminFrom() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLoadFromProcessEnvironment(t *testing.T) {
	t.Setenv(databaseURLEnv, "postgres://localhost/photos")
	t.Setenv(storageRootEnv, "/data")
	t.Setenv(fileBaseURLEnv, "https://files.example.test")
	t.Setenv(cursorHMACKeyEnv, strings.Repeat("x", 32))
	t.Setenv(apiAddrEnv, "127.0.0.1:8081")
	t.Setenv(logLevelEnv, "warn")
	t.Setenv(shutdownTimeoutEnv, "20s")

	if _, err := LoadAPI(); err != nil {
		t.Errorf("LoadAPI() error = %v", err)
	}
	if _, err := LoadWorker(); err != nil {
		t.Errorf("LoadWorker() error = %v", err)
	}
	if _, err := LoadAdmin(); err != nil {
		t.Errorf("LoadAdmin() error = %v", err)
	}
}

func TestMissingRequiredConfiguration(t *testing.T) {
	apiBase := map[string]string{
		databaseURLEnv: "postgres://localhost/photos", storageRootEnv: "/data", fileBaseURLEnv: "https://files.example.test",
		cursorHMACKeyEnv: strings.Repeat("x", 32),
	}
	tests := []struct {
		name string
		load func(map[string]string) error
		base map[string]string
		key  string
	}{
		{name: "api database", load: apiError, base: apiBase, key: databaseURLEnv},
		{name: "api storage", load: apiError, base: apiBase, key: storageRootEnv},
		{name: "api file URL", load: apiError, base: apiBase, key: fileBaseURLEnv},
		{name: "api cursor key", load: apiError, base: apiBase, key: cursorHMACKeyEnv},
		{name: "worker database", load: workerError, base: map[string]string{databaseURLEnv: "postgres://localhost/photos", storageRootEnv: "/data"}, key: databaseURLEnv},
		{name: "worker storage", load: workerError, base: map[string]string{databaseURLEnv: "postgres://localhost/photos", storageRootEnv: "/data"}, key: storageRootEnv},
		{name: "admin database", load: adminError, base: map[string]string{databaseURLEnv: "postgres://localhost/photos"}, key: databaseURLEnv},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := clone(tt.base)
			delete(vars, tt.key)
			err := tt.load(vars)
			if err == nil || !strings.Contains(err.Error(), tt.key+" is required") {
				t.Fatalf("error = %v, want missing %s", err, tt.key)
			}
		})
	}
}

func TestInvalidAPIConfiguration(t *testing.T) {
	valid := map[string]string{
		databaseURLEnv: "postgres://localhost/photos", storageRootEnv: "/data", fileBaseURLEnv: "https://files.example.test",
		cursorHMACKeyEnv: strings.Repeat("x", 32),
	}
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "relative storage", key: storageRootEnv, value: "data"},
		{name: "unclean storage dot", key: storageRootEnv, value: "/srv/./data"},
		{name: "unclean storage parent", key: storageRootEnv, value: "/srv/tmp/../data"},
		{name: "HTTP file URL", key: fileBaseURLEnv, value: "http://files.example.test"},
		{name: "relative file URL", key: fileBaseURLEnv, value: "files.example.test"},
		{name: "file URL user info", key: fileBaseURLEnv, value: "https://user:pass@files.example.test"},
		{name: "file URL query", key: fileBaseURLEnv, value: "https://files.example.test?q=secret"},
		{name: "file URL empty query", key: fileBaseURLEnv, value: "https://files.example.test?"},
		{name: "file URL fragment", key: fileBaseURLEnv, value: "https://files.example.test#secret"},
		{name: "short cursor key", key: cursorHMACKeyEnv, value: strings.Repeat("x", 31)},
		{name: "missing listen port", key: apiAddrEnv, value: "127.0.0.1"},
		{name: "empty listen host", key: apiAddrEnv, value: ":8080"},
		{name: "nonnumeric listen port", key: apiAddrEnv, value: "localhost:http"},
		{name: "zero listen port", key: apiAddrEnv, value: "localhost:0"},
		{name: "large listen port", key: apiAddrEnv, value: "localhost:65536"},
		{name: "invalid log level", key: logLevelEnv, value: "trace"},
		{name: "invalid database DSN", key: databaseURLEnv, value: "postgres://user:secret@ bad host/photos"},
		{name: "invalid duration", key: shutdownTimeoutEnv, value: "later"},
		{name: "short duration", key: shutdownTimeoutEnv, value: "999ms"},
		{name: "long duration", key: shutdownTimeoutEnv, value: "5m1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := clone(valid)
			vars[tt.key] = tt.value
			if _, err := LoadAPIFrom(env(vars)); err == nil {
				t.Fatal("LoadAPIFrom() error = nil")
			}
		})
	}
}

func TestInvalidWorkerAndAdminCommonConfiguration(t *testing.T) {
	tests := []struct {
		name string
		load func(map[string]string) error
		vars map[string]string
	}{
		{name: "worker relative root", load: workerError, vars: map[string]string{databaseURLEnv: "postgres://localhost/photos", storageRootEnv: "data"}},
		{name: "worker bad level", load: workerError, vars: map[string]string{databaseURLEnv: "postgres://localhost/photos", storageRootEnv: "/data", logLevelEnv: "verbose"}},
		{name: "worker bad duration", load: workerError, vars: map[string]string{databaseURLEnv: "postgres://localhost/photos", storageRootEnv: "/data", shutdownTimeoutEnv: "0s"}},
		{name: "admin bad level", load: adminError, vars: map[string]string{databaseURLEnv: "postgres://localhost/photos", logLevelEnv: "verbose"}},
		{name: "admin bad duration", load: adminError, vars: map[string]string{databaseURLEnv: "postgres://localhost/photos", shutdownTimeoutEnv: "6m"}},
		{name: "nil reader API", load: func(map[string]string) error { _, err := LoadAPIFrom(nil); return err }},
		{name: "nil reader worker", load: func(map[string]string) error { _, err := LoadWorkerFrom(nil); return err }},
		{name: "nil reader admin", load: func(map[string]string) error { _, err := LoadAdminFrom(nil); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.load(tt.vars); err == nil {
				t.Fatal("load error = nil")
			}
		})
	}
}

func TestErrorsAndLogAttrsRedactSecretsAndPaths(t *testing.T) {
	const databaseSecret = "postgres://admin:database-password@db/photos"
	const storagePath = "/private/photos"
	const cursorSecret = "cursor-secret-that-is-long-enough-1234"
	const urlSecret = "query-secret"

	_, err := LoadAPIFrom(env(map[string]string{
		databaseURLEnv: databaseSecret, storageRootEnv: storagePath,
		fileBaseURLEnv:   "https://files.example.test?token=" + urlSecret,
		cursorHMACKeyEnv: cursorSecret,
	}))
	assertOmits(t, err.Error(), databaseSecret, storagePath, cursorSecret, urlSecret)

	cfg, err := LoadAPIFrom(env(map[string]string{
		databaseURLEnv: databaseSecret, storageRootEnv: storagePath,
		fileBaseURLEnv: "https://files.example.test", cursorHMACKeyEnv: cursorSecret,
	}))
	if err != nil {
		t.Fatalf("LoadAPIFrom() error = %v", err)
	}
	text := attrsText(cfg.LogAttrs())
	assertOmits(t, text, databaseSecret, storagePath, cursorSecret, "files.example.test")

	worker, err := LoadWorkerFrom(env(map[string]string{databaseURLEnv: databaseSecret, storageRootEnv: storagePath}))
	if err != nil {
		t.Fatalf("LoadWorkerFrom() error = %v", err)
	}
	assertOmits(t, attrsText(worker.LogAttrs()), databaseSecret, storagePath)

	admin, err := LoadAdminFrom(env(map[string]string{databaseURLEnv: databaseSecret}))
	if err != nil {
		t.Fatalf("LoadAdminFrom() error = %v", err)
	}
	assertOmits(t, attrsText(admin.LogAttrs()), databaseSecret)
}

func TestInvalidDatabaseDSNErrorIsRedacted(t *testing.T) {
	const malformedSecret = "postgres://user:do-not-print@ bad host/photos"
	_, err := LoadAdminFrom(env(map[string]string{databaseURLEnv: malformedSecret}))
	if err == nil {
		t.Fatal("LoadAdminFrom() error = nil")
	}
	if strings.Contains(err.Error(), malformedSecret) || strings.Contains(err.Error(), "do-not-print") {
		t.Fatalf("error leaked database DSN: %q", err)
	}
	if !strings.Contains(err.Error(), databaseURLEnv) {
		t.Fatalf("error = %q, want environment variable name", err)
	}
}

func env(values map[string]string) Getenv {
	return func(key string) string { return values[key] }
}

func clone(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func apiError(values map[string]string) error {
	_, err := LoadAPIFrom(env(values))
	return err
}

func workerError(values map[string]string) error {
	_, err := LoadWorkerFrom(env(values))
	return err
}

func adminError(values map[string]string) error {
	_, err := LoadAdminFrom(env(values))
	return err
}

func attrsText(attrs []slog.Attr) string {
	var result strings.Builder
	for _, attr := range attrs {
		fmt.Fprintf(&result, "%s=%s ", attr.Key, attr.Value.String())
	}
	return result.String()
}

func assertOmits(t *testing.T, text string, forbidden ...string) {
	t.Helper()
	for _, value := range forbidden {
		if strings.Contains(text, value) {
			t.Errorf("text contains sensitive value %q: %q", value, text)
		}
	}
}
