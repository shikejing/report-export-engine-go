// Package reportexport is a server-side streaming report export engine.
//
// It turns continuously arriving data rows into spreadsheet files (XLSX) and,
// in the same export, comma-separated text (CSV). Both formats share one
// coercion pipeline and one display-string function, so their values and
// escaping semantics are identical. Exports stream rows through on-disk
// spools; peak memory is bounded by the configured batch size and does not
// grow with total row count.
//
// # Determinism
//
// Sheet order, column order, empty-sheet/empty-row handling, duplicate values,
// archive member order and number formatting are all fixed by the Report
// definition and the input sequence — never by buffer capacity, map
// iteration, or worker scheduling. Concurrent sheet export yields byte
// identical output to serial export.
//
// # Cancellation and cleanup
//
// Export honors context cancellation/deadline and Config.Timeout. Outputs are
// assembled in hidden temporary files and renamed into place only after a
// successful fsync; cancellation or failure removes every temporary file and
// never leaves a visible partial result.
//
// See README.md for the full semantics and configuration reference.
package reportexport
