package reportexport

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"encoding/xml"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	cfg.TempDir = t.TempDir()
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func sampleReport() Report {
	return Report{Sheets: []SheetConfig{
		{Name: "Sheet One", Columns: []Column{{Name: "id"}, {Name: "label"}, {Name: "v"}}},
		{Name: "Empty", Columns: []Column{{Name: "a"}, {Name: "b"}}},
	}}
}

// readXLSXGrid extracts all sheet grids from an xlsx by parsing inline
// strings, numbers, booleans and formula cached values. Dates are read back
// through their stored serial text (formatting is asserted separately).
func readXLSXGrid(t *testing.T, path string) [][][]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "xl/worksheets/sheet") && strings.HasSuffix(f.Name, ".xml") {
			names = append(names, f.Name)
		}
	}
	// positional order
	sortStringsByIndex(names)
	grids := make([][][]string, 0, len(names))
	for _, name := range names {
		var f *zip.File
		for _, cand := range zr.File {
			if cand.Name == name {
				f = cand
			}
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		dec := xml.NewDecoder(rc)
		var grid [][]string
		var row []string
		curCol := -1
		inV, inT := false, false
		cellBool := false
		var vbuf, tbuf strings.Builder
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			switch el := tok.(type) {
			case xml.StartElement:
				if el.Name.Local == "row" {
					row = nil
					curCol = -1
				}
				if el.Name.Local == "c" {
					curCol++
					for len(row) <= curCol {
						row = append(row, "")
					}
					cellBool = false
					for _, a := range el.Attr {
						if a.Name.Local == "t" && a.Value == "b" {
							cellBool = true
						}
					}
				}
				if el.Name.Local == "v" {
					inV = true
					vbuf.Reset()
				}
				if el.Name.Local == "t" {
					inT = true
					tbuf.Reset()
				}
			case xml.CharData:
				if inV {
					vbuf.Write(el)
				}
				if inT {
					tbuf.Write(el)
				}
			case xml.EndElement:
				switch el.Name.Local {
				case "v":
					inV = false
					val := vbuf.String()
					if cellBool {
						if val == "1" {
							val = "TRUE"
						} else if val == "0" {
							val = "FALSE"
						}
					}
					row[curCol] = val
				case "t":
					inT = false
					row[curCol] = tbuf.String()
				case "row":
					grid = append(grid, append([]string(nil), row...))
				}
			}
		}
		rc.Close()
		grids = append(grids, grid)
	}
	return grids
}

func sortStringsByIndex(in []string) {
	// insertion sort by the numeric index in sheetN.xml
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && sheetIndex(in[j]) < sheetIndex(in[j-1]); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

func sheetIndex(name string) int {
	name = strings.TrimPrefix(name, "xl/worksheets/sheet")
	name = strings.TrimSuffix(name, ".xml")
	n := 0
	for _, c := range name {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func readCSVGrid(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	return recs
}

func TestEndToEndBasic(t *testing.T) {
	e := testEngine(t, Config{BatchSize: 3, Concurrency: 2})
	h, err := e.NewReport(sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	w := h.Writer(0)
	rows := []Row{
		{1, "a,b", true},
		{2, `he said "hi"`, 3.5},
		{3, "line\nbreak", nil},
		{4, AsDateOnly(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)), AsFormula("A2*2", 4)},
		{5, struct{ X int }{X: 1}, []any{1}},
		{6, "six", math.NaN()},
	}
	for _, r := range rows {
		if err := w.WriteRow(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	res, err := h.Export(context.Background(), out, ExportOptions{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, err := os.Stat(res.XLSXPath); err != nil {
		t.Fatalf("xlsx missing: %v", err)
	}
	if len(res.CSVPaths) != 2 {
		t.Fatalf("want 2 csv files, got %d (%v)", len(res.CSVPaths), res.CSVPaths)
	}
	grids := readXLSXGrid(t, res.XLSXPath)
	if len(grids) != 2 {
		t.Fatalf("want 2 sheets, got %d", len(grids))
	}
	// header + 6 rows
	if len(grids[0]) != 7 {
		t.Fatalf("sheet1 rows=%d, want 7: %v", len(grids[0]), grids[0])
	}
	if grids[0][0][0] != "id" || grids[0][0][1] != "label" {
		t.Fatalf("bad header: %v", grids[0][0])
	}
	if grids[0][1][0] != "1" || grids[0][1][1] != "a,b" || grids[0][1][2] != "TRUE" {
		t.Fatalf("row1: %v", grids[0][1])
	}
	if grids[0][2][2] != "3.5" {
		t.Fatalf("float: %v", grids[0][2])
	}
	// empty sheet: header row only
	if len(grids[1]) != 1 || grids[1][0][0] != "a" {
		t.Fatalf("empty sheet grid: %v", grids[1])
	}
	// CSV of empty sheet is just header
	csvEmpty := readCSVGrid(t, res.CSVPaths["Empty"])
	if len(csvEmpty) != 1 {
		t.Fatalf("empty csv: %v", csvEmpty)
	}
	// warnings for unsupported struct and NaN
	if len(res.Warnings) < 2 {
		t.Fatalf("want >=2 warnings, got %+v", res.Warnings)
	}
	var reasons []string
	for _, wn := range res.Warnings {
		reasons = append(reasons, string(wn.Reason))
	}
	joined := strings.Join(reasons, ",")
	if !strings.Contains(joined, string(ReasonUnsupportedType)) {
		t.Fatalf("missing unsupported warning: %v", reasons)
	}
	if !strings.Contains(joined, string(ReasonNumberOutOfRange)) {
		t.Fatalf("missing out-of-range warning: %v", reasons)
	}
}

func TestInvalidConfig(t *testing.T) {
	cases := []Config{
		{BatchSize: -1}, {MaxSheets: -2}, {Concurrency: 0, Timeout: -1},
	}
	for i, c := range cases {
		c.TempDir = t.TempDir()
		if _, err := NewEngine(c); err == nil {
			t.Fatalf("case %d: want error", i)
		}
	}
}

func TestSheetLimitsAndValidation(t *testing.T) {
	e := testEngine(t, Config{MaxSheets: 2})
	bad := []Report{
		{},
		{Sheets: []SheetConfig{{Name: "a", Columns: []Column{{Name: "x"}}},
			{Name: "b", Columns: []Column{{Name: "x"}}},
			{Name: "c", Columns: []Column{{Name: "x"}}}}},
		{Sheets: []SheetConfig{{Name: "", Columns: []Column{{Name: "x"}}}}},
		{Sheets: []SheetConfig{{Name: "bad/name", Columns: []Column{{Name: "x"}}}}},
		{Sheets: []SheetConfig{{Name: "dup", Columns: []Column{{Name: "x"}}},
			{Name: "dup", Columns: []Column{{Name: "y"}}}}},
		{Sheets: []SheetConfig{{Name: "nocols"}}},
		{Sheets: []SheetConfig{{Name: strings.Repeat("x", 32), Columns: []Column{{Name: "x"}}}}},
	}
	for i, r := range bad {
		if _, err := e.NewReport(r); err == nil {
			t.Fatalf("case %d: want validation error", i)
		}
	}
}

func TestCSVXSXXConsistency(t *testing.T) {
	e := testEngine(t, Config{BatchSize: 7, Concurrency: 4})
	h, _ := e.NewReport(Report{Sheets: []SheetConfig{{Name: "s", Columns: []Column{{Name: "c1"}, {Name: "c2"}, {Name: "c3"}}}}})
	defer h.Close()
	w := h.Writer(0)
	values := []any{
		"plain", "with,comma", `quote"`, "tab\there", "newline\nx", "cr\rz",
		true, false, 1, -7, 0.0, 3.14159, "", nil, "  spaces  ",
		"\x00ctrl\x01", "unicode 日本語",
	}
	for _, v := range values {
		if err := w.WriteRow(context.Background(), Row{v, v, "x"}); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	res, err := h.Export(context.Background(), out, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	xg := readXLSXGrid(t, res.XLSXPath)[0]
	cg := readCSVGrid(t, res.CSVPaths["s"])
	if len(xg) != len(cg) {
		t.Fatalf("row count xlsx=%d csv=%d", len(xg), len(cg))
	}
	for i := range xg {
		for j := range xg[i] {
			if xg[i][j] != cg[i][j] {
				t.Fatalf("mismatch r%d c%d: xlsx=%q csv=%q", i, j, xg[i][j], cg[i][j])
			}
		}
	}
}

func TestDeterminismBatchAndConcurrency(t *testing.T) {
	build := func(t *testing.T, batch, conc int) ([]byte, map[string][]byte) {
		e := testEngine(t, Config{BatchSize: batch, Concurrency: conc})
		h, _ := e.NewReport(Report{Sheets: []SheetConfig{
			{Name: "s1", Columns: []Column{{Name: "a"}, {Name: "b"}}},
			{Name: "s2", Columns: []Column{{Name: "c"}}},
			{Name: "s3", Columns: []Column{{Name: "d"}}},
		}})
		for s, n := range []int{0, 50, 200} {
			w := h.Writer(s)
			for i := 0; i < n; i++ {
				if err := w.WriteRow(context.Background(), Row{i, "dup", i % 5}); err != nil {
					t.Fatal(err)
				}
			}
		}
		out := t.TempDir()
		res, err := h.Export(context.Background(), out, ExportOptions{})
		if err != nil {
			t.Fatal(err)
		}
		xlsxBytes, _ := os.ReadFile(res.XLSXPath)
		csvs := map[string][]byte{}
		for name, p := range res.CSVPaths {
			b, _ := os.ReadFile(p)
			csvs[name] = b
		}
		h.Close()
		return xlsxBytes, csvs
	}
	a, ac := build(t, 1, 1)
	b, bc := build(t, 137, 8)
	if string(a) != string(b) {
		t.Fatalf("xlsx bytes differ across batch/concurrency settings (%d vs %d bytes)", len(a), len(b))
	}
	for k := range ac {
		if string(ac[k]) != string(bc[k]) {
			t.Fatalf("csv %q differs", k)
		}
	}
}

func TestCancelCleansUp(t *testing.T) {
	e := testEngine(t, Config{BatchSize: 2, Concurrency: 1, Timeout: time.Nanosecond})
	h, _ := e.NewReport(Report{Sheets: []SheetConfig{
		{Name: "s1", Columns: []Column{{Name: "a"}}},
		{Name: "s2", Columns: []Column{{Name: "a"}}},
	}})
	defer h.Close()
	for i := 0; i < 1000; i++ {
		_ = h.Writer(0).WriteRow(context.Background(), Row{i})
		_ = h.Writer(1).WriteRow(context.Background(), Row{i})
	}
	out := t.TempDir()
	_, err := h.Export(context.Background(), out, ExportOptions{})
	if err == nil {
		t.Fatal("want cancellation error")
	}
	// no visible artifacts, no partial files left behind
	entries, _ := os.ReadDir(out)
	for _, en := range entries {
		t.Fatalf("leftover output artifact: %s", en.Name())
	}
	// spool files are released by Close (they must survive until then so a
	// failed export can be retried)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	root := e.cfg.TempDir
	if leftover, _ := os.ReadDir(root); len(leftover) != 0 {
		var names []string
		for _, n := range leftover {
			names = append(names, n.Name())
		}
		t.Fatalf("leftover temp entries: %v", names)
	}
}

func TestRerunAfterFailure(t *testing.T) {
	e := testEngine(t, Config{BatchSize: 5, Concurrency: 2})
	h, _ := e.NewReport(Report{Sheets: []SheetConfig{{Name: "s", Columns: []Column{{Name: "a"}}}}})
	defer h.Close()
	for i := 0; i < 50; i++ {
		_ = h.Writer(0).WriteRow(context.Background(), Row{i})
	}
	out := t.TempDir()
	// first attempt canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Export(ctx, out, ExportOptions{}); err == nil {
		t.Fatal("want error on canceled export")
	}
	// second attempt succeeds and equals a fresh single run
	res, err := h.Export(context.Background(), out, ExportOptions{Formats: []Format{FormatXLSX}})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	g := readXLSXGrid(t, res.XLSXPath)[0]
	if len(g) != 51 {
		t.Fatalf("rerun rows=%d want 51", len(g))
	}
	if g[50][0] != "49" {
		t.Fatalf("last row: %v", g[50])
	}
}

func TestLargeRowsBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	e := testEngine(t, Config{BatchSize: 128, Concurrency: 2})
	h, _ := e.NewReport(Report{Sheets: []SheetConfig{
		{Name: "big", Columns: []Column{{Name: "i"}, {Name: "pad"}, {Name: "dup"}}},
	}})
	defer h.Close()
	w := h.Writer(0)
	const total = 400_000
	payload := strings.Repeat("x", 64)
	for i := 0; i < total; i++ {
		if err := w.WriteRow(context.Background(), Row{i, payload, i % 10}); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	out := t.TempDir()
	res, err := h.Export(context.Background(), out, ExportOptions{Formats: []Format{FormatXLSX}})
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)
	// heap must stay far below the data size (400k * ~100B = 40MB+ rows alone)
	if m2.HeapAlloc > 120<<20 {
		t.Fatalf("heap too large after large export: %d MB", m2.HeapAlloc>>20)
	}
	if m1.HeapAlloc > 120<<20 {
		t.Fatalf("heap grew while streaming input: %d MB", m1.HeapAlloc>>20)
	}
	g := readXLSXGrid(t, res.XLSXPath)[0]
	if len(g) != total+1 {
		t.Fatalf("rows=%d want %d", len(g), total+1)
	}
	if g[total][0] != "399999" {
		t.Fatalf("last row value %q", g[total][0])
	}
}

func TestSpoolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sp, err := newSpool(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	in := [][]Cell{
		{{Kind: KindText, Text: "a,b\n"}, {Kind: KindNumber, Number: "1"}},
		{{Kind: KindBlank}, {Kind: KindBool, Bool: true}},
		{{Kind: KindFormula, Formula: "A1+1", Cached: &Cell{Kind: KindNumber, Number: "9"}}},
	}
	for _, r := range in {
		if err := sp.WriteRow(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	var got [][]Cell
	if err := sp.Rows(context.Background(), func(cells []Cell) error {
		got = append(got, cells)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d rows", len(got))
	}
	if got[0][0].Text != "a,b\n" || got[2][0].Cached.Number != "9" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sp.path); !os.IsNotExist(err) {
		t.Fatalf("spool file not removed: %v", err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("temp dir not empty: %v", es)
	}
}

func TestColLetter(t *testing.T) {
	cases := map[int]string{0: "A", 25: "Z", 26: "AA", 27: "AB", 701: "ZZ", 702: "AAA"}
	for n, want := range cases {
		if got := colLetter(n); got != want {
			t.Fatalf("colLetter(%d)=%s want %s", n, got, want)
		}
	}
}

var _ = filepath.Join
