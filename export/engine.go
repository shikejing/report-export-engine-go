package export

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Engine 报表导出引擎。配置在创建时校验并冻结。
type Engine struct {
	cfg Config
}

// NewEngine 创建引擎，配置非法时返回明确错误。
func NewEngine(cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Engine{cfg: cfg}, nil
}

// sheetState 单个工作表的导出状态。
type sheetState struct {
	def      SheetDef
	xlsx     *xlsxSheetWriter
	csv      *csvSheetWriter
	xmlPath  string
	csvPath  string
	rows     int64 // 数据行数（不含表头）
	pending  int   // 距上次 flush 的行数
	closed   bool
	degrades []DegradeEvent
	mu       sync.Mutex
}

// Export 单次导出会话。通过 SheetWriter 流式写行，
// Commit 原子产出最终结果，Abort 取消并清理。
type Export struct {
	cfg    Config
	defs   []SheetDef
	dir    string
	states []*sheetState
	byName map[string]int
	sem    chan struct{} // 限制并发写行的工作表数

	mu   sync.Mutex
	done bool // 已 Commit 或 Abort
}

// Export 开始一次导出：校验工作表定义，创建临时目录与各工作表的临时文件。
// 每个工作表的表头在此时写入，因此空表也会产出仅含表头的确定结果。
func (e *Engine) Export(ctx context.Context, sheets []SheetDef) (*Export, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(sheets) > e.cfg.MaxSheets {
		return nil, fmt.Errorf("%w: %d sheets declared, limit is %d", ErrTooManySheets, len(sheets), e.cfg.MaxSheets)
	}
	byName := make(map[string]int, len(sheets))
	for i, s := range sheets {
		if s.Name == "" {
			return nil, fmt.Errorf("%w: sheet %d has empty name", ErrInvalidSheet, i)
		}
		if len(s.Columns) == 0 {
			return nil, fmt.Errorf("%w: sheet %q has no columns", ErrInvalidSheet, s.Name)
		}
		if _, dup := byName[s.Name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateSheet, s.Name)
		}
		for j, c := range s.Columns {
			if c.Name == "" {
				return nil, fmt.Errorf("%w: sheet %q column %d has empty name", ErrInvalidSheet, s.Name, j)
			}
		}
		byName[s.Name] = i
	}

	dir, err := os.MkdirTemp(e.cfg.TempDir, "report-export-*")
	if err != nil {
		return nil, err
	}
	x := &Export{
		cfg:    e.cfg,
		defs:   sheets,
		dir:    dir,
		byName: byName,
		sem:    make(chan struct{}, e.cfg.Concurrency),
	}
	// 任何一步失败都必须清理临时目录。
	if err := x.initSheets(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return x, nil
}

func (x *Export) initSheets() error {
	for i, def := range x.defs {
		xmlPath := filepath.Join(x.dir, fmt.Sprintf("sheet%03d.xml", i+1))
		csvPath := filepath.Join(x.dir, fmt.Sprintf("sheet%03d.csv", i+1))
		xw, err := newXLSXSheetWriter(xmlPath)
		if err != nil {
			x.closeStates()
			return err
		}
		cw, err := newCSVSheetWriter(csvPath)
		if err != nil {
			xw.close()
			x.closeStates()
			return err
		}
		st := &sheetState{def: def, xlsx: xw, csv: cw, xmlPath: xmlPath, csvPath: csvPath}
		// 写表头行（两种格式一致：均为列名文本）。
		header := make([]cellValue, len(def.Columns))
		headerText := make([]string, len(def.Columns))
		for j, c := range def.Columns {
			header[j] = cellValue{typ: CellText, raw: c.Name}
			headerText[j] = c.Name
		}
		if err := xw.writeRow(1, header); err != nil {
			st.close()
			x.closeStates()
			return err
		}
		if err := cw.writeRow(headerText); err != nil {
			st.close()
			x.closeStates()
			return err
		}
		x.states = append(x.states, st)
	}
	return nil
}

func (x *Export) closeStates() {
	for _, st := range x.states {
		st.close()
	}
}

// SheetWriter 取得指定工作表的行写入器。
func (x *Export) SheetWriter(name string) (*SheetWriter, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.done {
		return nil, ErrExportClosed
	}
	idx, ok := x.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSheetNotFound, name)
	}
	return &SheetWriter{x: x, st: x.states[idx]}, nil
}

// SheetWriter 单工作表流式行写入器。同一 SheetWriter 可并发调用
// WriteRow（内部串行化），不同工作表的写入互不影响。
type SheetWriter struct {
	x  *Export
	st *sheetState
}

// WriteRow 追加一行；len(row) 必须等于列数。
// 行先按列定义分类与格式化，再同时写入 XLSX 与 CSV 临时文件，
// 因此两种格式逐条一致。达到批大小时落盘，峰值内存与总行数无关。
func (w *SheetWriter) WriteRow(ctx context.Context, row []any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(row) != len(w.st.def.Columns) {
		return fmt.Errorf("%w: sheet %q expects %d columns, got %d",
			ErrColumnCount, w.st.def.Name, len(w.st.def.Columns), len(row))
	}

	// 并发度信号量只限制同时在写的工作表数，不改变任何写入顺序。
	select {
	case w.x.sem <- struct{}{}:
		defer func() { <-w.x.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	w.st.mu.Lock()
	defer w.st.mu.Unlock()

	if w.x.isDone() {
		return ErrExportClosed
	}
	if w.st.closed {
		return ErrWriterClosed
	}
	if w.x.cfg.MaxRowsSheet > 0 && w.st.rows >= w.x.cfg.MaxRowsSheet {
		return fmt.Errorf("%w: sheet %q limit is %d rows", ErrTooManyRows, w.st.def.Name, w.x.cfg.MaxRowsSheet)
	}

	cells := make([]cellValue, len(row))
	texts := make([]string, len(row))
	for i, v := range row {
		cv, reason := classify(v, w.st.def.Columns[i].Type)
		if reason != "" {
			w.st.degrades = append(w.st.degrades, DegradeEvent{
				Sheet: w.st.def.Name, Row: w.st.rows, Column: i, Reason: reason,
			})
		}
		cells[i] = cv
		texts[i] = formatText(cv)
	}
	// 数据行从第 2 行开始（第 1 行为表头）。
	if err := w.st.xlsx.writeRow(w.st.rows+2, cells); err != nil {
		return err
	}
	if err := w.st.csv.writeRow(texts); err != nil {
		return err
	}
	w.st.rows++
	w.st.pending++
	if w.st.pending >= w.x.cfg.BatchSize {
		if err := w.st.xlsx.flush(); err != nil {
			return err
		}
		if err := w.st.csv.flush(); err != nil {
			return err
		}
		w.st.pending = 0
	}
	return nil
}

// Close 结束该工作表的写入（落盘缓冲）。幂等。
func (w *SheetWriter) Close() error {
	w.st.mu.Lock()
	defer w.st.mu.Unlock()
	if w.st.closed {
		return nil
	}
	w.st.closed = true
	return w.st.close()
}

func (st *sheetState) close() error {
	xerr := st.xlsx.close()
	cerr := st.csv.close()
	if xerr != nil {
		return xerr
	}
	return cerr
}

func (x *Export) isDone() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.done
}

// csvFileName 工作表 CSV 的确定命名：序号_名称.csv，非法字符替换为 _。
func csvFileName(index int, name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return fmt.Sprintf("%02d_%s.csv", index+1, b.String())
}

// Commit 收尾并原子地产出最终文件。outXLSX 为 xlsx 输出路径，
// csvDir 为 CSV 输出目录（每个工作表一个文件，按声明顺序编号命名）。
// 任何失败或取消都会清理全部临时文件与半成品输出，不留下可见残留。
func (x *Export) Commit(ctx context.Context, outXLSX, csvDir string) (*Result, error) {
	start := time.Now()
	x.mu.Lock()
	if x.done {
		x.mu.Unlock()
		return nil, ErrExportClosed
	}
	x.done = true
	x.mu.Unlock()

	// 无论成败，临时目录都必须清理。
	defer os.RemoveAll(x.dir)

	if err := ctx.Err(); err != nil {
		x.closeStates()
		return nil, err
	}

	// 1. 关闭所有工作表（落盘 footer 与缓冲）。
	for _, st := range x.states {
		st.mu.Lock()
		if !st.closed {
			st.closed = true
			if err := st.close(); err != nil {
				st.mu.Unlock()
				return nil, err
			}
		}
		st.mu.Unlock()
	}

	// 2. 产出 CSV：先写临时文件再 rename，失败时清理半成品。
	if err := os.MkdirAll(csvDir, 0o755); err != nil {
		return nil, err
	}
	csvPaths := make([]string, len(x.states))
	written := make([]string, 0, len(x.states))
	cleanupOnFail := func() {
		for _, p := range written {
			os.Remove(p)
		}
		os.Remove(outXLSX)
		os.Remove(outXLSX + ".tmp")
	}
	for i, st := range x.states {
		if err := ctx.Err(); err != nil {
			cleanupOnFail()
			return nil, err
		}
		dst := filepath.Join(csvDir, csvFileName(i, st.def.Name))
		if err := moveFile(st.csvPath, dst); err != nil {
			cleanupOnFail()
			return nil, err
		}
		csvPaths[i] = dst
		written = append(written, dst)
	}

	// 3. 组装 XLSX 到临时文件，再原子 rename。
	if err := ctx.Err(); err != nil {
		cleanupOnFail()
		return nil, err
	}
	xmlPaths := make([]string, len(x.states))
	for i, st := range x.states {
		xmlPaths[i] = st.xmlPath
	}
	if err := assembleXLSX(outXLSX+".tmp", x.defs, xmlPaths); err != nil {
		cleanupOnFail()
		return nil, err
	}
	if err := os.Rename(outXLSX+".tmp", outXLSX); err != nil {
		cleanupOnFail()
		return nil, err
	}

	res := &Result{XLSXPath: outXLSX, Duration: time.Since(start)}
	for i, st := range x.states {
		res.Sheets = append(res.Sheets, SheetResult{
			Name:     st.def.Name,
			Rows:     st.rows,
			CSVPath:  csvPaths[i],
			Degrades: st.degrades,
		})
	}
	return res, nil
}

// moveFile 把临时文件移动到最终位置：优先 rename，跨设备时复制+删除。
// 目标已存在时会被覆盖（重跑结果与一次成功导出一致）。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	tmp := dst + ".tmp"
	if err := copyFile(src, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Abort 取消导出并清理全部临时文件。幂等；Commit 后调用返回 ErrExportClosed。
func (x *Export) Abort() error {
	x.mu.Lock()
	if x.done {
		x.mu.Unlock()
		return ErrExportClosed
	}
	x.done = true
	x.mu.Unlock()
	x.closeStates()
	return os.RemoveAll(x.dir)
}
