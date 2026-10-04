package reportexport

import (
	"context"
	"time"
)

// Format identifies an output file format.
type Format string

const (
	// FormatXLSX is the Office Open XML spreadsheet format (.xlsx).
	FormatXLSX Format = "xlsx"
	// FormatCSV is comma-separated text (.csv), emitted once per worksheet.
	FormatCSV Format = "csv"
)

// Kind is the canonical cell type used across XLSX and CSV output.
type Kind int

const (
	KindBlank Kind = iota
	KindText
	KindNumber
	KindBool
	KindDate
	KindFormula
)

// Cell is the type-resolved, canonical representation of one cell value.
// Coercion happens once, before the value is spooled, so XLSX and CSV always
// observe identical semantics regardless of buffer size or flush order.
type Cell struct {
	Kind    Kind
	Text    string // text content; also the displayed value for CSV
	Number  string // canonical decimal representation for KindNumber
	Bool    bool
	Formula string  // formula source for KindFormula (without leading "=")
	Cached  *Cell   // cached result for KindFormula; nil => cached as blank
	Date    dateVal // serial number and date style for KindDate
}

type dateVal struct {
	Serial float64
	Date   bool // true: date-only style; false: date-time style
}

// Warning records a non-fatal degradation with a stable, distinguishable
// reason code and the location that triggered it.
type Warning struct {
	Sheet  string
	Row    int    // 1-based data row (0 for sheet-level warnings)
	Column string // column letter, "" when not applicable
	Reason Reason
	Detail string
}

// Row is one incoming data row: raw values in column order.
type Row []any

// SheetConfig defines a worksheet.
type SheetConfig struct {
	Name    string
	Columns []Column
}

// Column defines one worksheet column.
type Column struct {
	Name string
}

// Report is the unit of export: ordered sheets with independent columns.
type Report struct {
	Sheets []SheetConfig
}

// ExportOptions parameterizes one Export call.
type ExportOptions struct {
	// Formats selects output formats. Empty => both XLSX and CSV.
	Formats []Format
	// Name is the output base name (no extension). Default: "report". CSV
	// files are named "<base>-<sheetIndex>-<sheetName>.csv".
	Name string
	// Timeout overrides Config.Timeout for this call; 0 uses Config.Timeout.
	Timeout time.Duration
}

// ExportResult summarizes a finished export.
type ExportResult struct {
	// XLSXPath is set when FormatXLSX was requested.
	XLSXPath string
	// CSVPaths maps sheet name -> CSV path, in sheet order.
	CSVPaths map[string]string
	Warnings []Warning
}

// RowWriter accepts streaming rows for one sheet.
//
// Values are coerced lazily inside WriteRow: callers feed raw Go values
// (string, numeric, bool, time.Time, []byte, typed wrappers, nil, ...).
// Bytes accepted by the writer are immediately spooled; peak memory does not
// grow with total row count.
type RowWriter interface {
	WriteRow(ctx context.Context, row Row) error
}
