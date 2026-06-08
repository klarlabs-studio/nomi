package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

// Config holds database configuration
type Config struct {
	Path string
}

// appDataDir returns the OS-appropriate application data directory for Nomi.
// macOS:  ~/Library/Application Support/Nomi
// Windows: %APPDATA%\Nomi
// Linux:   ~/.config/nomi  (or $XDG_CONFIG_HOME/nomi)
//
// Honored env override (test/CI use): NOMI_DATA_DIR. When set to a
// non-empty path, every component that reads from appDataDir() (database,
// auth token, API endpoint marker) lands inside that directory instead of
// the OS app-data path. The journey-test runner relies on this to keep
// each run hermetic without hijacking the user's $HOME.
func appDataDir() string {
	if dir := os.Getenv("NOMI_DATA_DIR"); dir != "" {
		return dir
	}
	// Try UserConfigDir first (respects XDG_CONFIG_HOME on Linux)
	configDir, err := os.UserConfigDir()
	if err == nil {
		return filepath.Join(configDir, "Nomi")
	}

	// Fallback to home directory
	homeDir, err := os.UserHomeDir()
	if err == nil {
		// Use OS-appropriate hidden directory
		if runtime.GOOS == "darwin" {
			return filepath.Join(homeDir, "Library", "Application Support", "Nomi")
		}
		return filepath.Join(homeDir, ".nomi")
	}

	// Last resort: current working directory
	return ".nomi"
}

// DefaultConfig returns the default database configuration using the OS-native app data directory.
func DefaultConfig() Config {
	return Config{
		Path: filepath.Join(appDataDir(), "nomi.db"),
	}
}

// DB wraps sql.DB with Nomi-specific operations
type DB struct {
	*sql.DB
	config Config
}

// New creates a new database connection
func New(config Config) (*DB, error) {
	// Ensure directory exists
	dir := filepath.Dir(config.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	db, err := sql.Open("sqlite", config.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure SQLite for better concurrency and performance.
	// busy_timeout replaces the ad-hoc retry loops that were scattered across
	// a few repositories: the driver waits up to 5s for a contended lock
	// before returning SQLITE_BUSY, so repo code can treat queries as
	// ordinary operations.
	if _, err := db.Exec(`
		PRAGMA foreign_keys = ON;
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = NORMAL;
		PRAGMA busy_timeout = 5000;
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to configure database: %w", err)
	}

	// Single connection serialises every database operation through one
	// goroutine. This is the standard SQLite + Go pattern for a local-
	// first single-user app: query latency is microseconds so the lack of
	// reader parallelism is invisible, and serialising reads gives the
	// permission engine a clean TOCTOU window — assistant policy load and
	// approval decision can't interleave with a concurrent UpdateAssistant.
	//
	// Maintainers MUST avoid issuing a fresh r.db.Query inside an open
	// `rows.Next()` iteration: with one connection in the pool, the inner
	// query waits forever for a connection the outer iteration is holding.
	// PlanRepository.GetStepDefinitions historically tripped this; the fix
	// is to collect IDs first and batch-load child rows after the outer
	// rows.Close(). See loadDependenciesBatch for the canonical pattern.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	return &DB{
		DB:     db,
		config: config,
	}, nil
}

// Close closes the database connection
func (db *DB) Close() error {
	return db.DB.Close()
}

// Config returns the database configuration
func (db *DB) Config() Config {
	return db.config
}

// WithTx runs fn inside a database transaction. If fn returns an error or
// panics, the transaction is rolled back; otherwise it is committed. Used to
// couple state-machine row updates with their corresponding event inserts so
// a crash between the two can't produce a silent state-transition gap.
func (db *DB) WithTx(fn func(tx *sql.Tx) error) (err error) {
	tx, beginErr := db.Begin()
	if beginErr != nil {
		return fmt.Errorf("failed to begin transaction: %w", beginErr)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	err = fn(tx)
	return err
}
