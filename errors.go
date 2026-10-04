package reportexport

import "errors"

var (
	// ErrExportCanceled is returned when the context governing an export is
	// canceled (or its deadline expires) before the export completes.
	ErrExportCanceled = errors.New("reportexport: export canceled")
	// ErrTooManySheets is returned when a report exceeds Config.MaxSheets.
	ErrTooManySheets = errors.New("reportexport: sheet count exceeds configured maximum")
	// ErrInvalidConfig is returned for out-of-range configuration values.
	ErrInvalidConfig = errors.New("reportexport: invalid configuration")
	// ErrInvalidSheet is returned for malformed sheet/column definitions.
	ErrInvalidSheet = errors.New("reportexport: invalid sheet definition")
	// ErrUnknownFormat is returned when an unsupported format is requested.
	ErrUnknownFormat = errors.New("reportexport: unknown output format")
	// ErrClosed is returned when a closed writer is used.
	ErrClosed = errors.New("reportexport: writer already closed")
)
