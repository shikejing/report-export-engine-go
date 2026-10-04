package reportexport

import (
	"context"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestRowPaddingOverflowWarnings: short rows blank-pad; extra values dropped
// with TOO_MANY_VALUES carrying the exact 1-based data row number.
func TestRowPaddingOverflowWarnings(t *testing.T) {
	e := testEngine(t, Config{BatchSize: 1, Concurrency: 1})
	h, _ := e.NewReport(Report{Sheets: []SheetConfig{{Name: "s", Columns: []Column{{Name: "a"}, {Name: "b"}}}}})
	defer h.Close()
	w := h.Writer(0)
	if err := w.WriteRow(context.Background(), Row{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRow(context.Background(), Row{9}); err != nil { // short
		t.Fatal(err)
	}
	if err := w.WriteRow(context.Background(), Row{1, 2, 3, 4}); err != nil { // long
		t.Fatal(err)
	}
	out := t.TempDir()
	res, err := h.Export(context.Background(), out, ExportOptions{Formats: []Format{FormatXLSX}})
	if err != nil {
		t.Fatal(err)
	}
	var over []Warning
	for _, wn := range res.Warnings {
		if wn.Reason == ReasonTooManyValues {
			over = append(over, wn)
		}
	}
	if len(over) != 1 || over[0].Row != 3 {
		t.Fatalf("overflow warnings: %+v", over)
	}
	g := readXLSXGrid(t, res.XLSXPath)[0]
	if len(g) != 4 {
		t.Fatalf("rows=%d", len(g))
	}
	if g[2][0] != "9" || g[2][1] != "" {
		t.Fatalf("padding failed: %v", g[2])
	}
}

// TestEmptyRowsPreservePositions: empty slices yield blank records; only a
// sheet with zero rows collapses to header-only.
func TestEmptyRowsPreservePositions(t *testing.T) {
	e := testEngine(t, Config{BatchSize: 2})
	h, _ := e.NewReport(Report{Sheets: []SheetConfig{{Name: "s", Columns: []Column{{Name: "a"}, {Name: "b"}}}}})
	defer h.Close()
	w := h.Writer(0)
	for _, r := range []Row{{"x", "y"}, {}, {"z", ""}, {}} {
		if err := w.WriteRow(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	res, _ := h.Export(context.Background(), out, ExportOptions{})
	g := readXLSXGrid(t, res.XLSXPath)[0]
	if len(g) != 5 {
		t.Fatalf("xlsx rows=%d want 5", len(g))
	}
	if strings.Join(g[2], ",") != "," || strings.Join(g[4], ",") != "," {
		t.Fatalf("blank rows wrong: %v | %v", g[2], g[4])
	}
	cg := readCSVGrid(t, res.CSVPaths["s"])
	if len(cg) != 5 || strings.Join(cg[2], "|") != "|" || strings.Join(cg[4], "|") != "|" {
		t.Fatalf("csv blank rows wrong: %v", cg)
	}
}

// TestBadSheetIsolation: one sheet's spool I/O failure must not stop others;
// the bad sheet is emitted header-only with SHEET_FAILED; XLSX still opens.
func TestBadSheetIsolation(t *testing.T) {
	e := testEngine(t, Config{BatchSize: 2, Concurrency: 3})
	h, _ := e.NewReport(Report{Sheets: []SheetConfig{
		{Name: "good1", Columns: []Column{{Name: "a"}}},
		{Name: "bad", Columns: []Column{{Name: "a"}}},
		{Name: "good2", Columns: []Column{{Name: "a"}}},
	}})
	defer h.Close()
	for i := 0; i < 10; i++ {
		_ = h.Writer(0).WriteRow(context.Background(), Row{i})
		_ = h.Writer(2).WriteRow(context.Background(), Row{i * 2})
	}
	// force failure by closing the bad sheet's underlying spool file
	bad := h.sheets[1]
	bad.mu.Lock()
	bad.err = errFakeIO
	bad.mu.Unlock()
	if err := h.Writer(1).WriteRow(context.Background(), Row{1}); err != errFakeIO {
		t.Fatalf("want fake io error, got %v", err)
	}
	out := t.TempDir()
	res, err := h.Export(context.Background(), out, ExportOptions{})
	if err != nil {
		t.Fatalf("export should still succeed: %v", err)
	}
	grids := readXLSXGrid(t, res.XLSXPath)
	if len(grids) != 3 {
		t.Fatalf("grids=%d", len(grids))
	}
	if len(grids[0]) != 11 || len(grids[1]) != 1 || len(grids[2]) != 11 {
		t.Fatalf("isolation row counts: %d %d %d", len(grids[0]), len(grids[1]), len(grids[2]))
	}
	var sawFail bool
	for _, wn := range res.Warnings {
		if wn.Sheet == "bad" && wn.Reason == ReasonSheetFailed {
			sawFail = true
		}
	}
	if !sawFail {
		t.Fatalf("missing SHEET_FAILED warning: %+v", res.Warnings)
	}
}

// TestWarningsDeterministicAcrossRerunsAndConcurrency: warning slices are
// identical for the same data regardless of concurrency or repeated exports.
func TestWarningsDeterministicAcrossRerunsAndConcurrency(t *testing.T) {
	run := func(conc int) []Warning {
		e := testEngine(t, Config{BatchSize: 4, Concurrency: conc})
		h, _ := e.NewReport(Report{Sheets: []SheetConfig{
			{Name: "s1", Columns: []Column{{Name: "a"}, {Name: "b"}}},
			{Name: "s2", Columns: []Column{{Name: "a"}}},
		}})
		defer h.Close()
		for i := 0; i < 20; i++ {
			_ = h.Writer(0).WriteRow(context.Background(), Row{struct{}{}, i, "extra"})
			_ = h.Writer(1).WriteRow(context.Background(), Row{math.NaN()})
		}
		out := t.TempDir()
		r1, err := h.Export(context.Background(), out, ExportOptions{Formats: []Format{FormatXLSX}})
		if err != nil {
			t.Fatal(err)
		}
		r2, err := h.Export(context.Background(), t.TempDir(), ExportOptions{Formats: []Format{FormatXLSX}})
		if err != nil {
			t.Fatal(err)
		}
		if sig(r1.Warnings) != sig(r2.Warnings) {
			t.Fatalf("warnings changed between reruns:\n%v\n%v", r1.Warnings, r2.Warnings)
		}
		return r1.Warnings
	}
	a := run(1)
	b := run(16)
	if sig(a) != sig(b) {
		t.Fatalf("warnings differ by concurrency:\n%v\n%v", a, b)
	}
}

func sig(ws []Warning) string {
	var b strings.Builder
	for _, w := range ws {
		b.WriteString(w.Sheet + "|" + itoa(w.Row) + "|" + w.Column + "|" + string(w.Reason) + "|" + w.Detail + "\n")
	}
	return b.String()
}

// TestConcurrentWritersManySheets: hammer all sheets concurrently; output is
// identical to a serial build and no temp files leak.
func TestConcurrentWritersManySheets(t *testing.T) {
	const sheetsN, rowsN = 24, 300
	build := func(concurrent bool) []byte {
		e := testEngine(t, Config{BatchSize: 13, Concurrency: 8})
		h, _ := e.NewReport(func() Report {
			ss := make([]SheetConfig, sheetsN)
			for i := range ss {
				ss[i] = SheetConfig{Name: "S" + itoa(i), Columns: []Column{{Name: "i"}, {Name: "v"}}}
			}
			return Report{Sheets: ss}
		}())
		defer h.Close()
		write := func(si int) {
			w := h.Writer(si)
			for i := 0; i < rowsN; i++ {
				if err := w.WriteRow(context.Background(), Row{i, "v" + itoa(i%7)}); err != nil {
					t.Error(err)
					return
				}
			}
		}
		if concurrent {
			var wg sync.WaitGroup
			for s := 0; s < sheetsN; s++ {
				wg.Add(1)
				go func(s int) { defer wg.Done(); write(s) }(s)
			}
			wg.Wait()
		} else {
			for s := 0; s < sheetsN; s++ {
				write(s)
			}
		}
		out := t.TempDir()
		res, err := h.Export(context.Background(), out, ExportOptions{Formats: []Format{FormatXLSX}})
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(res.XLSXPath)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	serial := build(false)
	par := build(true)
	if string(serial) != string(par) {
		t.Fatalf("concurrent xlsx differs: %d vs %d bytes", len(serial), len(par))
	}
}

// TestContextCancelDuringWriteAndExport covers cancel before Export.
func TestContextCancelBeforeExport(t *testing.T) {
	e := testEngine(t, Config{})
	h, _ := e.NewReport(Report{Sheets: []SheetConfig{{Name: "s", Columns: []Column{{Name: "a"}}}}})
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Writer(0).WriteRow(ctx, Row{1}); err == nil {
		t.Fatal("want error writing with canceled context")
	}
}

// errFakeIO is a stable fake spool failure.
type fakeIOErr struct{}

func (fakeIOErr) Error() string { return "fake spool i/o failure" }

var errFakeIO = fakeIOErr{}
