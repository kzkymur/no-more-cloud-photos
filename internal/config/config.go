package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultAPIAddr         = "127.0.0.1:8080"
	DefaultLogLevel        = "info"
	DefaultShutdownTimeout = 30 * time.Second
	MinShutdownTimeout     = time.Second
	MaxShutdownTimeout     = 5 * time.Minute
)

const (
	databaseURLEnv     = "NMCP_DATABASE_URL"
	storageRootEnv     = "NMCP_STORAGE_ROOT"
	fileBaseURLEnv     = "NMCP_FILE_BASE_URL"
	cursorHMACKeyEnv   = "NMCP_CURSOR_HMAC_KEY"
	apiAddrEnv         = "NMCP_API_ADDR"
	logLevelEnv        = "NMCP_LOG_LEVEL"
	shutdownTimeoutEnv = "NMCP_SHUTDOWN_TIMEOUT"
)

// Getenv makes configuration loading deterministic in tests and embedders.
type Getenv func(string) string

type APIConfig struct {
	DatabaseURL     string
	StorageRoot     string
	FileBaseURL     string
	CursorHMACKey   []byte
	Addr            string
	LogLevel        string
	ShutdownTimeout time.Duration
}

type WorkerConfig struct {
	DatabaseURL     string
	StorageRoot     string
	LogLevel        string
	ShutdownTimeout time.Duration
}

type AdminConfig struct {
	DatabaseURL     string
	LogLevel        string
	ShutdownTimeout time.Duration
}

func LoadAPI() (APIConfig, error)       { return LoadAPIFrom(os.Getenv) }
func LoadWorker() (WorkerConfig, error) { return LoadWorkerFrom(os.Getenv) }
func LoadAdmin() (AdminConfig, error)   { return LoadAdminFrom(os.Getenv) }

func LoadAPIFrom(getenv Getenv) (APIConfig, error) {
	common, err := loadCommon(getenv)
	if err != nil {
		return APIConfig{}, err
	}
	storageRoot, err := required(getenv, storageRootEnv)
	if err != nil {
		return APIConfig{}, err
	}
	if err := validateStorageRoot(storageRoot); err != nil {
		return APIConfig{}, err
	}
	fileBaseURL, err := required(getenv, fileBaseURLEnv)
	if err != nil {
		return APIConfig{}, err
	}
	if err := validateFileBaseURL(fileBaseURL); err != nil {
		return APIConfig{}, err
	}
	cursorKey, err := required(getenv, cursorHMACKeyEnv)
	if err != nil {
		return APIConfig{}, err
	}
	if len([]byte(cursorKey)) < 32 {
		return APIConfig{}, fmt.Errorf("%s must contain at least 32 bytes", cursorHMACKeyEnv)
	}
	addr := valueOrDefault(getenv, apiAddrEnv, DefaultAPIAddr)
	if err := validateListenAddr(addr); err != nil {
		return APIConfig{}, err
	}

	return APIConfig{
		DatabaseURL:     common.databaseURL,
		StorageRoot:     storageRoot,
		FileBaseURL:     fileBaseURL,
		CursorHMACKey:   []byte(cursorKey),
		Addr:            addr,
		LogLevel:        common.logLevel,
		ShutdownTimeout: common.shutdownTimeout,
	}, nil
}

func LoadWorkerFrom(getenv Getenv) (WorkerConfig, error) {
	common, err := loadCommon(getenv)
	if err != nil {
		return WorkerConfig{}, err
	}
	storageRoot, err := required(getenv, storageRootEnv)
	if err != nil {
		return WorkerConfig{}, err
	}
	if err := validateStorageRoot(storageRoot); err != nil {
		return WorkerConfig{}, err
	}

	return WorkerConfig{
		DatabaseURL:     common.databaseURL,
		StorageRoot:     storageRoot,
		LogLevel:        common.logLevel,
		ShutdownTimeout: common.shutdownTimeout,
	}, nil
}

func LoadAdminFrom(getenv Getenv) (AdminConfig, error) {
	common, err := loadCommon(getenv)
	if err != nil {
		return AdminConfig{}, err
	}
	return AdminConfig{
		DatabaseURL:     common.databaseURL,
		LogLevel:        common.logLevel,
		ShutdownTimeout: common.shutdownTimeout,
	}, nil
}

// LogAttrs returns only operationally useful, non-secret configuration.
func (c APIConfig) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("listen_addr", c.Addr),
		slog.String("log_level", c.LogLevel),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
	}
}

// LogAttrs deliberately excludes the database URL and storage path.
func (c WorkerConfig) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("log_level", c.LogLevel),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
	}
}

// LogAttrs deliberately excludes the database URL.
func (c AdminConfig) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("log_level", c.LogLevel),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
	}
}

type commonConfig struct {
	databaseURL     string
	logLevel        string
	shutdownTimeout time.Duration
}

func loadCommon(getenv Getenv) (commonConfig, error) {
	if getenv == nil {
		return commonConfig{}, fmt.Errorf("configuration environment reader is nil")
	}
	databaseURL, err := required(getenv, databaseURLEnv)
	if err != nil {
		return commonConfig{}, err
	}
	if _, err := pgxpool.ParseConfig(databaseURL); err != nil {
		return commonConfig{}, fmt.Errorf("%s must be a valid PostgreSQL DSN", databaseURLEnv)
	}
	logLevel := strings.ToLower(valueOrDefault(getenv, logLevelEnv, DefaultLogLevel))
	if !validLogLevel(logLevel) {
		return commonConfig{}, fmt.Errorf("%s must be one of debug, info, warn, or error", logLevelEnv)
	}
	shutdownTimeout := DefaultShutdownTimeout
	if raw := getenv(shutdownTimeoutEnv); raw != "" {
		shutdownTimeout, err = time.ParseDuration(raw)
		if err != nil {
			return commonConfig{}, fmt.Errorf("%s must be a valid duration", shutdownTimeoutEnv)
		}
	}
	if shutdownTimeout < MinShutdownTimeout || shutdownTimeout > MaxShutdownTimeout {
		return commonConfig{}, fmt.Errorf("%s must be between %s and %s", shutdownTimeoutEnv, MinShutdownTimeout, MaxShutdownTimeout)
	}
	return commonConfig{databaseURL: databaseURL, logLevel: logLevel, shutdownTimeout: shutdownTimeout}, nil
}

func required(getenv Getenv, name string) (string, error) {
	if getenv == nil {
		return "", fmt.Errorf("configuration environment reader is nil")
	}
	value := getenv(name)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func valueOrDefault(getenv Getenv, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}

func validateStorageRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("%s must be an absolute path", storageRootEnv)
	}
	if filepath.Clean(root) != root {
		return fmt.Errorf("%s must be a clean path", storageRootEnv)
	}
	return nil
}

func validateFileBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("%s must be an absolute HTTPS URL", fileBaseURLEnv)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("%s must not contain user information, a query, or a fragment", fileBaseURLEnv)
	}
	return nil
}

func validateListenAddr(addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil || host == "" || strings.ContainsAny(host, " /\t\r\n") {
		return fmt.Errorf("%s must be a host:port address", apiAddrEnv)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s must contain a port between 1 and 65535", apiAddrEnv)
	}
	return nil
}

func validLogLevel(level string) bool {
	switch level {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}
