package reportexport

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

// Config controls export behavior. Zero fields are replaced by defaults in
// NewEngine; explicit values are validated before use.
type Config struct {
	// BatchSize is the number of rows buffered in memory before a flush to
	// the on-disk spool (and the read batch during merge). Must be > 0.
	// Default: 1000.
	BatchSize int

	// MaxSheets limits how many worksheets one workbook may contain.
	// Must be > 0. Default: 64.
	MaxSheets int

	// Concurrency is the maximum number of worksheets exported in parallel.
	// Serial export is Concurrency == 1. Must be > 0.
	// Default: runtime.NumCPU().
	Concurrency int

	// Timeout bounds one Export call. 0 means no timeout; callers can still
	// cancel via context. Must be >= 0.
	Timeout time.Duration

	// TempDir holds spool and in-progress output files. Default:
	// os.TempDir(). The directory must exist and be writable.
	TempDir string
}

// Default configuration values applied by NewEngine to zero fields.
const (
	DefaultBatchSize = 1000
	DefaultMaxSheets = 64
)

// Validate checks configuration boundaries. Invalid configuration returns a
// descriptive error wrapping ErrInvalidConfig; no partial state is created.
func (c Config) Validate() error {
	if c.BatchSize < 0 {
		return fmt.Errorf("%w: BatchSize must be >= 0, got %d", ErrInvalidConfig, c.BatchSize)
	}
	if c.MaxSheets < 0 {
		return fmt.Errorf("%w: MaxSheets must be >= 0, got %d", ErrInvalidConfig, c.MaxSheets)
	}
	if c.Concurrency < 0 {
		return fmt.Errorf("%w: Concurrency must be >= 0, got %d", ErrInvalidConfig, c.Concurrency)
	}
	if c.Timeout < 0 {
		return fmt.Errorf("%w: Timeout must be >= 0, got %s", ErrInvalidConfig, c.Timeout)
	}
	return nil
}

// withDefaults returns a copy of cfg with zero fields replaced by defaults.
func withDefaults(cfg Config) Config {
	out := cfg
	if out.BatchSize == 0 {
		out.BatchSize = DefaultBatchSize
	}
	if out.MaxSheets == 0 {
		out.MaxSheets = DefaultMaxSheets
	}
	if out.Concurrency == 0 {
		out.Concurrency = runtime.NumCPU()
		if out.Concurrency < 1 {
			out.Concurrency = 1
		}
	}
	if out.TempDir == "" {
		out.TempDir = os.TempDir()
	}
	return out
}
