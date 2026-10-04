package reportexport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// sheetJob tracks one worksheet during an export.
//
// Bad rows never set err: they degrade to flagged cells during coercion and
// only append a Warning. err is set solely when the spool itself fails
// (I/O error) or the incoming context is already done at write time. During
// merge a failed sheet is emitted header-only with ReasonSheetFailed, so a
// single bad sheet never prevents the rest of the workbook from exporting.
type sheetJob struct {
	cfg   SheetConfig
	spool *spool

	mu       sync.Mutex
	closed   bool
	err      error
	warnings []Warning
}

// sheetWriter is the RowWriter for one sheet. It coerces rows immediately and
// hands canonical cells to the spool, so only a bounded batch ever resides in
// memory regardless of how many rows arrive.
type sheetWriter struct {
	job   *sheetJob
	batch []Cell
}

// addWarnings appends cell/row warnings under the job lock. Warnings stay in
// append (write) order, which is also row order for a single writer.
func (j *sheetJob) addWarnings(ws ...Warning) {
	j.mu.Lock()
	j.warnings = append(j.warnings, ws...)
	j.mu.Unlock()
}

func (w *sheetWriter) WriteRow(ctx context.Context, row Row) error {
	j := w.job
	j.mu.Lock()
	if j.err != nil {
		err := j.err
		j.mu.Unlock()
		return err
	}
	j.mu.Unlock()

	if err := ctx.Err(); err != nil {
		j.mu.Lock()
		j.err = err
		j.mu.Unlock()
		return err
	}
	n := len(j.cfg.Columns)
	cells := make([]Cell, n)
	var ws []Warning
	for i := 0; i < n; i++ {
		if i < len(row) {
			c, reason, detail := inferCell(row[i])
			cells[i] = c
			if reason != "" {
				ws = append(ws, Warning{
					Sheet:  j.cfg.Name,
					Row:    -1, // stamped with the 1-based row by the caller
					Column: colLetter(i),
					Reason: reason,
					Detail: detail,
				})
			}
		}
	}
	if len(row) > n {
		ws = append(ws, Warning{
			Sheet:  j.cfg.Name,
			Row:    -1,
			Reason: ReasonTooManyValues,
			Detail: fmt.Sprintf("row carried %d values, sheet has %d columns", len(row), n),
		})
	}
	if len(ws) > 0 {
		j.addWarnings(ws...)
	}
	if err := j.spool.WriteRow(ctx, cells); err != nil {
		j.mu.Lock()
		j.err = err
		j.mu.Unlock()
		return err
	}
	return nil
}

// Engine executes report exports. One Engine is safe for concurrent use by
// multiple goroutines; each NewReport/Export uses its own temp directory.
type Engine struct {
	cfg Config
}

// NewEngine validates cfg, applies defaults to zero fields, and returns a
// ready engine. Invalid configuration yields ErrInvalidConfig with detail.
func NewEngine(cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = withDefaults(cfg)
	if fi, err := os.Stat(cfg.TempDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: TempDir is not an existing directory: %s", ErrInvalidConfig, cfg.TempDir)
	}
	return &Engine{cfg: cfg}, nil
}

var invalidSheetNameChars = regexp.MustCompile(`[\\/?*\[\]:]`)

// NewReport registers a workbook and opens its per-sheet spools. Sheet order,
// column order, names and empty-sheet behavior are fixed by rpt.
func (e *Engine) NewReport(rpt Report) (*ReportHandle, error) {
	if len(rpt.Sheets) == 0 {
		return nil, fmt.Errorf("%w: report must contain at least one sheet", ErrInvalidSheet)
	}
	if len(rpt.Sheets) > e.cfg.MaxSheets {
		return nil, fmt.Errorf("%w: %d sheets, maximum is %d", ErrTooManySheets, len(rpt.Sheets), e.cfg.MaxSheets)
	}
	names := make(map[string]int, len(rpt.Sheets))
	for i, sh := range rpt.Sheets {
		if sh.Name == "" {
			return nil, fmt.Errorf("%w: sheet %d has an empty name", ErrInvalidSheet, i)
		}
		if len([]rune(sh.Name)) > 31 {
			return nil, fmt.Errorf("%w: sheet %d name is %d runes long, Excel limit is 31", ErrInvalidSheet, i, len([]rune(sh.Name)))
		}
		if invalidSheetNameChars.MatchString(sh.Name) {
			return nil, fmt.Errorf("%w: sheet %d name %q contains one of \\ / ? * [ ] :", ErrInvalidSheet, i, sh.Name)
		}
		if prev, ok := names[sh.Name]; ok {
			return nil, fmt.Errorf("%w: duplicate sheet name %q (sheets %d and %d)", ErrInvalidSheet, sh.Name, prev, i)
		}
		names[sh.Name] = i
		if len(sh.Columns) == 0 {
			return nil, fmt.Errorf("%w: sheet %d (%q) must define at least one column", ErrInvalidSheet, i, sh.Name)
		}
		for j, c := range sh.Columns {
			if c.Name == "" {
				return nil, fmt.Errorf("%w: sheet %q column %d has an empty name", ErrInvalidSheet, sh.Name, j)
			}
		}
	}

	dir, err := spoolDir(e.cfg.TempDir)
	if err != nil {
		return nil, err
	}
	h := &ReportHandle{
		engine:  e,
		rpt:     rpt,
		dir:     dir,
		sheets:  make([]*sheetJob, len(rpt.Sheets)),
		writers: make([]RowWriter, len(rpt.Sheets)),
	}
	for i, sh := range rpt.Sheets {
		sp, err := newSpool(dir, e.cfg.BatchSize)
		if err != nil {
			h.Close()
			return nil, err
		}
		job := &sheetJob{cfg: sh, spool: sp}
		h.sheets[i] = job
		h.writers[i] = &rowNumberingWriter{inner: &sheetWriter{job: job}, job: job}
	}
	return h, nil
}

// rowNumberingWriter stamps warnings with the 1-based data row number as rows
// are written, keeping cell-coercion warnings location-accurate without
// holding rows back for batching.
type rowNumberingWriter struct {
	inner *sheetWriter
	job   *sheetJob
	n     int
}

func (w *rowNumberingWriter) WriteRow(ctx context.Context, row Row) error {
	w.n++
	start := len(w.job.warnings)
	err := w.inner.WriteRow(ctx, row)
	w.job.mu.Lock()
	for i := start; i < len(w.job.warnings); i++ {
		if w.job.warnings[i].Row == -1 {
			w.job.warnings[i].Row = w.n
		}
	}
	w.job.mu.Unlock()
	return err
}

// ReportHandle is one in-flight workbook.
type ReportHandle struct {
	engine  *Engine
	rpt     Report
	dir     string
	sheets  []*sheetJob
	writers []RowWriter
	closed  bool
	mu      sync.Mutex
}

// Writer returns the streaming row writer for sheet index i.
func (h *ReportHandle) Writer(i int) RowWriter {
	if i < 0 || i >= len(h.writers) {
		return nil
	}
	return h.writers[i]
}

// Export runs the configured concurrent fan-out and the deterministic merge.
//
// Outputs are written as hidden "*.partial" files in outDir and renamed into
// place only after every selected format finished and fsynced; cancellation,
// timeout or any error removes all partial and final files produced by this
// call, so no half-finished artifact is ever visible. Calling Export again
// after a failed attempt replays the same spools and produces the same result
// as a single successful run (spools are not consumed until Close).
func (h *ReportHandle) Export(ctx context.Context, outDir string, opts ExportOptions) (*ExportResult, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	h.mu.Unlock()

	formats := opts.Formats
	if len(formats) == 0 {
		formats = []Format{FormatXLSX, FormatCSV}
	}
	for _, f := range formats {
		if f != FormatXLSX && f != FormatCSV {
			return nil, fmt.Errorf("%w: %q", ErrUnknownFormat, string(f))
		}
	}
	if fi, err := os.Stat(outDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("reportexport: output directory does not exist: %s", outDir)
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = h.engine.cfg.Timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return nil, mapCanceled(err)
	}

	name := opts.Name
	if name == "" {
		name = "report"
	}
	wantXLSX := hasFormat(formats, FormatXLSX)
	wantCSV := hasFormat(formats, FormatCSV)

	res := &ExportResult{CSVPaths: make(map[string]string)}
	var planned []string
	cleanup := func() {
		for _, p := range planned {
			os.Remove(p)
			os.Remove(p + ".partial")
		}
	}

	// Deterministic warnings: always collect in sheet order, then row order,
	// independent of worker scheduling.
	res.Warnings = h.collectWarnings()

	// Fan out CSV rendering (one independent task per sheet). Jobs are
	// dispatched and collected in sheet order; the semaphore only limits how
	// many run at once, never which output bytes appear where.
	var csvPaths []string
	if wantCSV {
		sem := make(chan struct{}, h.engine.cfg.Concurrency)
		var wg sync.WaitGroup
		errs := make([]error, len(h.sheets))
		csvPaths = make([]string, len(h.sheets))
		for i, job := range h.sheets {
			path := filepath.Join(outDir, csvFileName(name, i, job.cfg.Name))
			csvPaths[i] = path
			planned = append(planned, path)
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, job *sheetJob, path string) {
				defer wg.Done()
				defer func() { <-sem }()
				job.mu.Lock()
				failed := job.err != nil
				job.mu.Unlock()
				errs[i] = writeCSV(ctx, h.dir, path, h.rpt.Sheets[i].Columns, job.spool, h.engine.cfg.BatchSize, failed)
			}(i, job, path)
		}
		wg.Wait()
		if err := firstInOrder(errs); err != nil {
			cleanup()
			return nil, mapCanceled(err)
		}
		if err := ctx.Err(); err != nil {
			cleanup()
			return nil, mapCanceled(err)
		}
	}

	// Merge XLSX last, always replaying sheets in definition order so the
	// archive is identical whether CSV workers ran concurrently or serially.
	if wantXLSX {
		xlsxPath := filepath.Join(outDir, name+".xlsx")
		planned = append(planned, xlsxPath)
		if err := mergeXLSX(ctx, xlsxPath, &h.rpt, h.sheets, h.engine.cfg.BatchSize); err != nil {
			cleanup()
			return nil, mapCanceled(err)
		}
		res.XLSXPath = xlsxPath
	}
	if wantCSV {
		for i, p := range csvPaths {
			res.CSVPaths[h.rpt.Sheets[i].Name] = p
		}
	}
	return res, nil
}

func (h *ReportHandle) collectWarnings() []Warning {
	var out []Warning
	for _, job := range h.sheets {
		job.mu.Lock()
		failed := job.err != nil
		ws := append([]Warning(nil), job.warnings...)
		job.mu.Unlock()
		if failed {
			ws = append(ws, Warning{
				Sheet:  job.cfg.Name,
				Reason: ReasonSheetFailed,
				Detail: job.err.Error(),
			})
		}
		out = append(out, ws...)
	}
	return out
}

func hasFormat(fs []Format, f Format) bool {
	for _, x := range fs {
		if x == f {
			return true
		}
	}
	return false
}

func firstInOrder(errs []error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func mapCanceled(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrExportCanceled, err)
	}
	return err
}

var csvUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// csvFileName builds the deterministic per-sheet CSV name:
// "<base>-<1-based index>-<sanitized sheet name>.csv".
func csvFileName(base string, idx int, sheet string) string {
	safe := csvUnsafe.ReplaceAllString(strings.TrimSpace(sheet), "_")
	safe = strings.Trim(safe, "._-")
	if safe == "" {
		safe = "sheet"
	}
	return fmt.Sprintf("%s-%02d-%s.csv", base, idx+1, safe)
}

// Close releases all spool files and the per-export temp directory. It is
// idempotent and safe to call after a failed or canceled Export.
func (h *ReportHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	var firstErr error
	for _, job := range h.sheets {
		if job != nil && job.spool != nil {
			if err := job.spool.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if h.dir != "" {
		if err := os.RemoveAll(h.dir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
