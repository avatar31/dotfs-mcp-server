// Package config centralises runtime configuration for the MCP server.
//
// Every value is sourced from an environment variable so that the binary can be
// launched identically by an MCP client (Claude Desktop / Cursor), a systemd
// unit or a container runtime.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Default values applied when the matching environment variable is unset.
const (
	DefaultWorkspaceRoot = "./workspace"
	DefaultCacheDir      = "./agent_knowledge"
	DefaultHTTPAddr      = "127.0.0.1:8080"
	DefaultMaxFileSize   = 2 << 20 // 2 MiB
	DefaultServerName    = "dotfs-mcp-server"
	DefaultServerVersion = "1.0.0"
)

// Config is the fully resolved, validated runtime configuration.
type Config struct {
	// WorkspaceRoot is the shared parent directory holding one sub-directory
	// per repository (e.g. <root>/auth-service-go, <root>/packet-router-c).
	WorkspaceRoot string
	// CacheDir is the on-disk location of the embedded BadgerDB LSM store.
	CacheDir string
	// HTTPAddr is the listen address of the management REST API.
	HTTPAddr string
	// APIToken, when non-empty, is required as "Authorization: Bearer <token>"
	// on every management API call.
	APIToken string
	// CapabilitiesFile optionally points at a JSON repository capability matrix.
	CapabilitiesFile string
	// MaxFileSize caps the size of a single source file that will be parsed.
	MaxFileSize int64
	// IndexOnStart triggers a full workspace index during boot.
	IndexOnStart bool
	// EnableHTTP toggles the management REST API.
	EnableHTTP bool
	// SkipDirs are directory names pruned during the workspace walk.
	SkipDirs []string
	// GCInterval controls BadgerDB value-log garbage collection cadence.
	GCInterval time.Duration
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
	// ServerName / ServerVersion are advertised during the MCP handshake.
	ServerName    string
	ServerVersion string
}

// Load reads the environment, applies defaults and validates the result.
func Load() (Config, error) {
	cfg := Config{
		WorkspaceRoot:    envString("DOTFS_WORKSPACE_ROOT", DefaultWorkspaceRoot),
		CacheDir:         envString("DOTFS_CACHE_DB", DefaultCacheDir),
		HTTPAddr:         envString("DOTFS_HTTP_ADDR", DefaultHTTPAddr),
		APIToken:         os.Getenv("DOTFS_API_TOKEN"),
		CapabilitiesFile: os.Getenv("DOTFS_CAPABILITIES_FILE"),
		LogLevel:         envString("DOTFS_LOG_LEVEL", "info"),
		ServerName:       envString("DOTFS_SERVER_NAME", DefaultServerName),
		ServerVersion:    envString("DOTFS_SERVER_VERSION", DefaultServerVersion),
		SkipDirs:         envList("DOTFS_SKIP_DIRS", []string{".git", ".svn", ".hg", "node_modules", "vendor", "third_party", "build", "dist", "out", ".idea", ".vscode"}),
	}

	var err error
	if cfg.MaxFileSize, err = envInt64("DOTFS_MAX_FILE_SIZE", DefaultMaxFileSize); err != nil {
		return Config{}, err
	}
	if cfg.IndexOnStart, err = envBool("DOTFS_INDEX_ON_START", true); err != nil {
		return Config{}, err
	}
	if cfg.EnableHTTP, err = envBool("DOTFS_HTTP_ENABLED", true); err != nil {
		return Config{}, err
	}
	if cfg.GCInterval, err = envDuration("DOTFS_GC_INTERVAL", 10*time.Minute); err != nil {
		return Config{}, err
	}

	if cfg.WorkspaceRoot, err = filepath.Abs(cfg.WorkspaceRoot); err != nil {
		return Config{}, fmt.Errorf("resolve DOTFS_WORKSPACE_ROOT: %w", err)
	}
	if cfg.CacheDir, err = filepath.Abs(cfg.CacheDir); err != nil {
		return Config{}, fmt.Errorf("resolve DOTFS_CACHE_DB: %w", err)
	}

	return cfg, cfg.Validate()
}

// Validate reports whether the configuration can be used to boot the server.
func (c Config) Validate() error {
	info, err := os.Stat(c.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("workspace root %q is not readable: %w", c.WorkspaceRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace root %q is not a directory", c.WorkspaceRoot)
	}
	if c.MaxFileSize <= 0 {
		return fmt.Errorf("DOTFS_MAX_FILE_SIZE must be greater than zero, got %d", c.MaxFileSize)
	}
	if c.GCInterval <= 0 {
		return fmt.Errorf("DOTFS_GC_INTERVAL must be greater than zero, got %s", c.GCInterval)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("DOTFS_LOG_LEVEL must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	if c.EnableHTTP && strings.TrimSpace(c.HTTPAddr) == "" {
		return fmt.Errorf("DOTFS_HTTP_ADDR must not be empty when the management API is enabled")
	}
	return nil
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envList(key string, fallback []string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt64(key string, fallback int64) (int64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return v, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", key, err)
	}
	return v, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return v, nil
}
