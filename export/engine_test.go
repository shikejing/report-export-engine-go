package export

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// runExport 执行一次完整导出并返回产物字节，便于逐字节比较。
func runExport(t *testing.T, cfg Config, sheets []SheetDef, rows map[string][][]any) (xlsxBytes []byte, csvBytes map[string][]byte, res *Result) {
	t.Helper()
	eng, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	dir := t.TempDir()
	x, err := eng.Export(context.Background(), sheets)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	for _, sd := range sheets {
		w, err := x.SheetWriter(sd.Name)
		if err != nil {
			t.Fatalf("SheetWriter(%q): %v", sd.Name, err)
		}
		for i, r := range rows[sd.Name] {
			if err := w.WriteRow(context.Background(), r); err != nil {
				t.Fatalf("WriteRow sheet=%q row=%d: %v", sd.Name, i, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	outXLSX := filepath.Join(dir, "out.xlsx")
	csvDir := filepath.Join(dir, "csv")
	res, err = x.Commit(context.Background(), outXLSX, csvDir)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	xlsxBytes, err = os.ReadFile(outXLSX)
	if err != nil {
		t.Fatalf("read xlsx: %v", err)
	}
	csvBytes = map[string][]byte{}
	for _, sr := range res.Sheets {
		b, err := os.ReadFile(sr.CSVPath)
		if err != nil {
			t.Fatalf("read csv: %v", err)
		}
		csvBytes[sr.Name] = b
	}
	// Commit 后临时目录必须已清理。
	left, err := os.ReadDir(cfg.TempDir)
	if err == nil {
		for _, e := range left {
			if strings.HasPrefix(e.Name(), "report-export-") {
				t.Fatalf("temp dir not cleaned: %s", e.Name())
			}
		}
	}
	return xlsxBytes, csvBytes, res
}

func demoSheets() []SheetDef {
	return []SheetDef{
		{Name: "orders", Columns: []Column{
			{Name: "id"}, {Name: "amount"}, {Name: "paid"}, {Name: "created_at"}, {Name: "note"},
		}},
		{Name: "summary", Columns: []Column{
			{Name: "label"}, {Name: "total"}, {Name: "ratio"},
		}},
	}
}

func demoRows(n int) map[string][][]any {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := map[string][][]any{}
	for i := 0; i < n; i++ {
		rows["orders"] = append(rows["orders"], []any{
			i, float64(i) * 1.5, i%2 == 0, base.Add(time.Duration(i) * time.Minute),
			fmt.Sprintf("note,%d \"quoted\"\nline", i%7),
		})
		if i%3 == 0 {
			rows["summary"] = append(rows["summary"], []any{
				fmt.Sprintf("grp-%d", i%5), i, "=A1*2",
			})
		}
	}
	return rows
}

// 大行数导出：峰值内存不随行数增长（通过流式落盘保证），
// 这里验证 20 万行可以完成且行数正确。
func TestLargeRowCount(t *testing.T) {
	cfg := Config{BatchSize: 500, TempDir: t.TempDir()}
	n := 200000
	_, csvBytes, res := runExport(t, cfg, demoSheets(), demoRows(n))
	if res.Sheets[0].Rows != int64(n) {
		t.Fatalf("rows = %d, want %d", res.Sheets[0].Rows, n)
	}
	// 字段内含换行，必须用 CSV 解析器统计记录数。
	cr := csv.NewReader(strings.NewReader(string(csvBytes["orders"])))
	recs, err := cr.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n+1 { // 表头 + 数据行
		t.Fatalf("csv records = %d, want %d", len(recs), n+1)
	}
}

// 批切分不变性：同样的数据按不同 BatchSize 输入，产物逐字节一致。
func TestBatchSizeInvariance(t *testing.T) {
	sheets := demoSheets()
	rows := demoRows(5000)
	var refX []byte
	var refC map[string][]byte
	for _, bs := range []int{1, 7, 64, 100000} {
		xb, cb, _ := runExport(t, Config{BatchSize: bs, TempDir: t.TempDir()}, sheets, rows)
		if refX == nil {
			refX, refC = xb, cb
			continue
		}
		if string(xb) != string(refX) {
			t.Fatalf("xlsx differs for BatchSize=%d", bs)
		}
		for name, b := range cb {
			if string(b) != string(refC[name]) {
				t.Fatalf("csv %q differs for BatchSize=%d", name, bs)
			}
		}
	}
}

// 重复值：不使用共享字符串缓存，结果与缓存容量/遍历顺序无关。
func TestDuplicateValuesDeterministic(t *testing.T) {
	sheets := []SheetDef{{Name: "s", Columns: []Column{{Name: "v"}}}}
	mkRows := func() map[string][][]any {
		var rs [][]any
		for i := 0; i < 20000; i++ {
			rs = append(rs, []any{fmt.Sprintf("dup-%d", i%10)}) // 大量重复值
		}
		return map[string][][]any{"s": rs}
	}
	x1, c1, _ := runExport(t, Config{BatchSize: 3, TempDir: t.TempDir()}, sheets, mkRows())
	x2, c2, _ := runExport(t, Config{BatchSize: 9999, TempDir: t.TempDir()}, sheets, mkRows())
	if string(x1) != string(x2) || string(c1["s"]) != string(c2["s"]) {
		t.Fatal("duplicate values: output depends on batch/cache size")
	}
}

// 多工作表：顺序确定、空表空行行为确定、单表坏数据不影响其它表。
func TestMultiSheetSemantics(t *testing.T) {
	sheets := []SheetDef{
		{Name: "first", Columns: []Column{{Name: "a"}}},
		{Name: "empty", Columns: []Column{{Name: "b"}, {Name: "c"}}},
		{Name: "third", Columns: []Column{{Name: "x"}, {Name: "y"}}},
	}
	rows := map[string][][]any{
		"first": {{struct{ N int }{9}}, {"ok"}}, // 第一表含坏数据（降级）
		"empty": {},                             // 空表：只有表头
		"third": {{nil, nil}, {1, "v"}},         // 空行 + 正常行
	}
	xlsxBytes, csvBytes, res := runExport(t, Config{BatchSize: 2, TempDir: t.TempDir()}, sheets, rows)

	// 工作表顺序与声明一致。
	wb := unzipEntry(t, xlsxBytes, "xl/workbook.xml")
	i1 := strings.Index(wb, `name="first"`)
	i2 := strings.Index(wb, `name="empty"`)
	i3 := strings.Index(wb, `name="third"`)
	if !(0 < i1 && i1 < i2 && i2 < i3) {
		t.Fatalf("sheet order wrong in workbook.xml: %s", wb)
	}
	// 空表 CSV 只有表头。
	if got := string(csvBytes["empty"]); got != "b,c\n" {
		t.Fatalf("empty sheet csv = %q", got)
	}
	// 空行：CSV 中表现为空字段行。
	if got := string(csvBytes["third"]); got != "x,y\n,\n1,v\n" {
		t.Fatalf("third csv = %q", got)
	}
	// 坏数据降级：记录了原因，且导出成功。
	if len(res.Sheets[0].Degrades) != 1 || res.Sheets[0].Degrades[0].Reason != ReasonUnsupportedKind {
		t.Fatalf("degrades = %+v", res.Sheets[0].Degrades)
	}
	// 第一表的坏数据不影响第三表。
	if res.Sheets[2].Rows != 2 {
		t.Fatalf("third rows = %d", res.Sheets[2].Rows)
	}
}

// 并发导出：多个工作表并发写，产物与单线程顺序写完全一致。
func TestConcurrentExportMatchesSequential(t *testing.T) {
	sheets := demoSheets()
	rows := demoRows(20000)

	// 顺序基线。
	seqX, seqC, _ := runExport(t, Config{BatchSize: 128, TempDir: t.TempDir()}, sheets, rows)

	// 并发：每个工作表一个 goroutine，行交错到达。
	cfg := Config{BatchSize: 128, Concurrency: 8, TempDir: t.TempDir()}
	eng, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	x, err := eng.Export(context.Background(), sheets)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, sd := range sheets {
		sd := sd
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := x.SheetWriter(sd.Name)
			if err != nil {
				t.Error(err)
				return
			}
			for _, r := range rows[sd.Name] {
				if err := w.WriteRow(context.Background(), r); err != nil {
					t.Error(err)
					return
				}
			}
			if err := w.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	dir := t.TempDir()
	outXLSX := filepath.Join(dir, "c.xlsx")
	csvDir := filepath.Join(dir, "csv")
	if _, err := x.Commit(context.Background(), outXLSX, csvDir); err != nil {
		t.Fatal(err)
	}
	gotX, _ := os.ReadFile(outXLSX)
	if string(gotX) != string(seqX) {
		t.Fatal("concurrent xlsx differs from sequential")
	}
	for _, sd := range sheets {
		got, _ := os.ReadFile(filepath.Join(csvDir, csvFileName(indexOf(sheets, sd.Name), sd.Name)))
		if string(got) != string(seqC[sd.Name]) {
			t.Fatalf("concurrent csv %q differs from sequential", sd.Name)
		}
	}
}

func indexOf(sheets []SheetDef, name string) int {
	for i, s := range sheets {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// 取消：写入中途取消，Abort 清理临时文件，不留下可见半成品。
func TestCancelAndCleanup(t *testing.T) {
	tmp := t.TempDir()
	outDir := t.TempDir()
	cfg := Config{BatchSize: 10, TempDir: tmp}
	eng, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	x, err := eng.Export(context.Background(), demoSheets())
	if err != nil {
		t.Fatal(err)
	}
	w, _ := x.SheetWriter("orders")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows := demoRows(1000)["orders"]
	cancelled := false
	for i, r := range rows {
		if i == 50 {
			cancel()
		}
		if err := w.WriteRow(ctx, r); err != nil {
			cancelled = true
			break
		}
	}
	if !cancelled {
		t.Fatal("WriteRow did not observe cancellation")
	}
	if err := x.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// 临时目录无残留。
	left, _ := os.ReadDir(tmp)
	if len(left) != 0 {
		t.Fatalf("temp dir not clean after Abort: %v", left)
	}
	// 输出目录无半成品。
	outLeft, _ := os.ReadDir(outDir)
	if len(outLeft) != 0 {
		t.Fatalf("output dir has leftovers: %v", outLeft)
	}
	// Abort 后再写/提交必须报明确错误。
	if err := w.WriteRow(context.Background(), rows[0]); err == nil {
		t.Fatal("WriteRow after Abort should fail")
	}
	if _, err := x.Commit(context.Background(), filepath.Join(outDir, "x.xlsx"), filepath.Join(outDir, "csv")); err == nil {
		t.Fatal("Commit after Abort should fail")
	}
}

// Commit 时上下文已取消：不得产出任何可见文件。
func TestCommitWithCancelledContext(t *testing.T) {
	outDir := t.TempDir()
	eng, _ := NewEngine(Config{TempDir: t.TempDir()})
	x, err := eng.Export(context.Background(), demoSheets())
	if err != nil {
		t.Fatal(err)
	}
	w, _ := x.SheetWriter("orders")
	for _, r := range demoRows(100)["orders"] {
		if err := w.WriteRow(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := x.Commit(ctx, filepath.Join(outDir, "x.xlsx"), filepath.Join(outDir, "csv")); err == nil {
		t.Fatal("Commit with cancelled ctx should fail")
	}
	left, _ := os.ReadDir(outDir)
	if len(left) != 0 {
		t.Fatalf("leftover files after cancelled commit: %v", left)
	}
}

// 重跑一致性：同样的输入导出两次，产物逐字节一致。
func TestRerunIdentical(t *testing.T) {
	sheets := demoSheets()
	rows := demoRows(3000)
	x1, c1, _ := runExport(t, Config{BatchSize: 100, TempDir: t.TempDir()}, sheets, rows)
	x2, c2, _ := runExport(t, Config{BatchSize: 100, TempDir: t.TempDir()}, sheets, rows)
	if string(x1) != string(x2) {
		t.Fatal("rerun xlsx differs")
	}
	for n := range c1 {
		if string(c1[n]) != string(c2[n]) {
			t.Fatalf("rerun csv %q differs", n)
		}
	}
}

// 配置校验：非法配置给出明确报错。
func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{
		{BatchSize: -1},
		{MaxSheets: -2},
		{Concurrency: 0 - 3},
		{MaxRowsSheet: -1},
	} {
		if _, err := NewEngine(cfg); err == nil {
			t.Fatalf("config %+v should be rejected", cfg)
		}
	}
	// 零值配置使用默认值。
	eng, err := NewEngine(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if eng.cfg.BatchSize != DefaultBatchSize || eng.cfg.MaxSheets != DefaultMaxSheets || eng.cfg.Concurrency != DefaultConcurrency {
		t.Fatalf("defaults not applied: %+v", eng.cfg)
	}
}

// 边界：工作表数量上限、行数上限、列数不匹配、重复表名。
func TestLimits(t *testing.T) {
	eng, _ := NewEngine(Config{MaxSheets: 1, TempDir: t.TempDir()})
	if _, err := eng.Export(context.Background(), demoSheets()); err == nil {
		t.Fatal("should reject too many sheets")
	}
	if _, err := eng.Export(context.Background(), []SheetDef{
		{Name: "a", Columns: []Column{{Name: "x"}}},
		{Name: "a", Columns: []Column{{Name: "y"}}},
	}); err == nil {
		t.Fatal("should reject duplicate sheet names")
	}

	eng2, _ := NewEngine(Config{MaxRowsSheet: 5, TempDir: t.TempDir()})
	x, err := eng2.Export(context.Background(), []SheetDef{{Name: "s", Columns: []Column{{Name: "v"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer x.Abort()
	w, _ := x.SheetWriter("s")
	for i := 0; i < 5; i++ {
		if err := w.WriteRow(context.Background(), []any{i}); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	if err := w.WriteRow(context.Background(), []any{5}); err == nil {
		t.Fatal("should reject rows beyond MaxRowsSheet")
	}
	if err := w.WriteRow(context.Background(), []any{1, 2}); err == nil {
		t.Fatal("should reject column count mismatch")
	}
}

// CSV 转义：含逗号、引号、换行的值能被标准解析器还原，且与 XLSX 内联文本一致。
func TestCSVAndXLSXValueConsistency(t *testing.T) {
	sheets := []SheetDef{{Name: "s", Columns: []Column{{Name: "v"}}}}
	tricky := []string{"a,b", `say "hi"`, "line1\nline2", "=1+1", "  padded  ", "中文,👋"}
	var rows [][]any
	for _, s := range tricky {
		rows = append(rows, []any{s})
	}
	xlsxBytes, csvBytes, _ := runExport(t, Config{TempDir: t.TempDir()}, sheets, map[string][][]any{"s": rows})

	// CSV 解析还原。
	r := csv.NewReader(strings.NewReader(string(csvBytes["s"])))
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != len(tricky)+1 {
		t.Fatalf("csv records = %d", len(recs))
	}
	for i, s := range tricky {
		if recs[i+1][0] != s {
			t.Fatalf("csv row %d = %q, want %q", i, recs[i+1][0], s)
		}
	}
	// XLSX 内联字符串包含同样的文本（XML 转义后）。
	sheetXML := unzipEntry(t, xlsxBytes, "xl/worksheets/sheet1.xml")
	for _, want := range []string{"a,b", "say &quot;hi&quot;", "line1\nline2", "  padded  ", "中文,👋"} {
		if !strings.Contains(sheetXML, want) {
			t.Fatalf("sheet xml missing %q", want)
		}
	}
	// 公式在 XLSX 中是 <f> 元素。
	if !strings.Contains(sheetXML, "<f>1+1</f>") {
		t.Fatalf("formula not written as <f>: %s", sheetXML)
	}
}

// unzipEntry 读取 xlsx（zip）中的指定条目。
func unzipEntry(t *testing.T, xlsxBytes []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(strings.NewReader(string(xlsxBytes)), int64(len(xlsxBytes)))
	if err != nil {
		t.Fatalf("open xlsx zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatalf("entry %q not found in xlsx", name)
	return ""
}

// 超时：带 deadline 的上下文在写入期间到期，行为与取消一致。
func TestTimeoutDuringWrite(t *testing.T) {
	tmp := t.TempDir()
	eng, _ := NewEngine(Config{BatchSize: 5, TempDir: tmp})
	x, err := eng.Export(context.Background(), demoSheets())
	if err != nil {
		t.Fatal(err)
	}
	w, _ := x.SheetWriter("orders")
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // 确保已超时
	err = w.WriteRow(ctx, demoRows(1)["orders"][0])
	if err == nil {
		t.Fatal("WriteRow with expired deadline should fail")
	}
	if err := x.Abort(); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(tmp)
	if len(left) != 0 {
		t.Fatalf("temp dir not clean: %v", left)
	}
}

// 表头与数据在两种格式中逐条一致（含日期、布尔、数值、公式）。
func TestHeaderAndTypedValuesConsistency(t *testing.T) {
	sheets := []SheetDef{{Name: "s", Columns: []Column{
		{Name: "n"}, {Name: "b"}, {Name: "d"}, {Name: "f"},
	}}}
	tm := time.Date(2024, 6, 1, 8, 30, 0, 0, time.FixedZone("X", 3600))
	rows := map[string][][]any{"s": {
		{1.5, true, tm, "=A2*2"},
	}}
	xlsxBytes, csvBytes, _ := runExport(t, Config{TempDir: t.TempDir()}, sheets, rows)
	got := string(csvBytes["s"])
	want := "n,b,d,f\n1.5,true,2024-06-01T07:30:00Z,=A2*2\n"
	if got != want {
		t.Fatalf("csv = %q, want %q", got, want)
	}
	xml := unzipEntry(t, xlsxBytes, "xl/worksheets/sheet1.xml")
	// 表头行 + 数据行都在，日期为序列值、公式为 <f>。
	for _, frag := range []string{`<row r="1">`, `<row r="2">`, `<f>A2*2</f>`, `t="b"`} {
		if !strings.Contains(xml, frag) {
			t.Fatalf("sheet xml missing %q: %s", frag, xml)
		}
	}
}
