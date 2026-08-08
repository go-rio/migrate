package migrate

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	defaultTable       = "schema_migrations"
	defaultLockTimeout = time.Minute
)

// Clock supplies timestamps for migration records.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Option configures a Migrator.
type Option func(*config)

type config struct {
	collection     *Collection
	table          string
	lock           bool
	lockTimeout    time.Duration
	strictChecksum bool
	safety         SafetyLevel
	logger         *slog.Logger
	clock          Clock
}

func defaultConfig() config {
	return config{
		collection:  defaultCollection,
		table:       defaultTable,
		lock:        true,
		lockTimeout: defaultLockTimeout,
		logger:      slog.New(slog.DiscardHandler),
		clock:       systemClock{},
	}
}

// WithCollection replaces the package-level default collection.
func WithCollection(c *Collection) Option {
	return func(cfg *config) { cfg.collection = c }
}

// WithTable sets the records table name and advisory-lock namespace.
func WithTable(name string) Option {
	return func(cfg *config) { cfg.table = name }
}

// WithoutLock disables serialization. Use it only with an external single-run
// guarantee.
func WithoutLock() Option {
	return func(cfg *config) { cfg.lock = false }
}

// WithLockTimeout sets the advisory-lock wait; the default is one minute.
func WithLockTimeout(d time.Duration) Option {
	return func(cfg *config) { cfg.lockTimeout = d }
}

// WithStrictChecksum makes Up fail with ErrChecksumMismatch instead of
// warning. Repair accepts reviewed drift.
func WithStrictChecksum() Option {
	return func(cfg *config) { cfg.strictChecksum = true }
}

// WithLogger sets the progress logger; the default discards logs.
func WithLogger(l *slog.Logger) Option {
	return func(cfg *config) { cfg.logger = l }
}

// WithClock replaces the migration-record time source.
func WithClock(c Clock) Option {
	return func(cfg *config) { cfg.clock = c }
}

func (c *config) validate() error {
	if c.collection == nil {
		return errors.New("migrate: collection must not be nil")
	}
	if c.table == "" || len(c.table) > 128 || strings.ContainsAny(c.table, "`\"'\x00") {
		return fmt.Errorf("migrate: invalid records table name %q", c.table)
	}
	if c.lockTimeout <= 0 {
		return errors.New("migrate: lock timeout must be positive")
	}
	if c.logger == nil {
		return errors.New("migrate: logger must not be nil")
	}
	if c.clock == nil {
		c.clock = systemClock{}
	}
	return nil
}
