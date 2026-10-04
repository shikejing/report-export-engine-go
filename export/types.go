// Package export 提供流式报表导出引擎：将持续到达的数据行导出为
// XLSX 电子表格与 CSV 文本，两种格式共享同一套取值与转义语义。
package export

import (
	"errors"
	"fmt"
	"time"
)

// CellType 单元格逻辑类型。
type CellType int

const (
	CellEmpty   CellType = iota // 空单元格
	CellText                    // 文本
	CellNumber                  // 数值
	CellBool                    // 布尔
	CellDate                    // 日期时间
	CellFormula                 // 公式
)

func (t CellType) String() string {
	switch t {
	case CellEmpty:
		return "empty"
	case CellText:
		return "text"
	case CellNumber:
		return "number"
	case CellBool:
		return "bool"
	case CellDate:
		return "date"
	case CellFormula:
		return "formula"
	default:
		return "unknown"
	}
}

// Column 单个工作表的一列定义。
type Column struct {
	Name string   // 表头显示名，必填
	Type CellType // 强制类型；CellEmpty 表示按值自动识别
}

// SheetDef 工作表定义：名称与列。
type SheetDef struct {
	Name    string
	Columns []Column
}

// Config 导出配置。
type Config struct {
	BatchSize    int    // 每个工作表内存中缓冲的最大行数，达到即落盘；<=0 使用默认值
	MaxSheets    int    // 单次导出允许的最大工作表数；<=0 使用默认值
	Concurrency  int    // 并发导出的工作表上限；<=0 使用默认值
	TempDir      string // 临时文件目录；空表示使用系统默认临时目录
	MaxRowsSheet int64  // 单工作表最大行数（不含表头）；<=0 表示不限制
}

// 默认配置常量。
const (
	DefaultBatchSize   = 1024
	DefaultMaxSheets   = 64
	DefaultConcurrency = 4
)

// 配置与输入校验错误。
var (
	ErrInvalidConfig  = errors.New("export: invalid config")
	ErrTooManySheets  = errors.New("export: too many sheets")
	ErrTooManyRows    = errors.New("export: too many rows in sheet")
	ErrDuplicateSheet = errors.New("export: duplicate sheet name")
	ErrInvalidSheet   = errors.New("export: invalid sheet definition")
	ErrExportClosed   = errors.New("export: export already finished")
	ErrSheetNotFound  = errors.New("export: sheet not found")
	ErrWriterClosed   = errors.New("export: sheet writer already closed")
	ErrColumnCount    = errors.New("export: row column count mismatch")
)

// Validate 校验并归一化配置（填充默认值）。非法配置返回明确错误。
func (c *Config) Validate() error {
	if c.BatchSize == 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.BatchSize < 0 {
		return fmt.Errorf("%w: BatchSize must be > 0, got %d", ErrInvalidConfig, c.BatchSize)
	}
	if c.MaxSheets == 0 {
		c.MaxSheets = DefaultMaxSheets
	}
	if c.MaxSheets < 0 {
		return fmt.Errorf("%w: MaxSheets must be > 0, got %d", ErrInvalidConfig, c.MaxSheets)
	}
	if c.Concurrency == 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.Concurrency < 0 {
		return fmt.Errorf("%w: Concurrency must be > 0, got %d", ErrInvalidConfig, c.Concurrency)
	}
	if c.MaxRowsSheet < 0 {
		return fmt.Errorf("%w: MaxRowsSheet must be >= 0, got %d", ErrInvalidConfig, c.MaxRowsSheet)
	}
	return nil
}

// DegradeReason 取值无法按预期类型识别时的降级原因。
type DegradeReason string

const (
	ReasonUnsupportedKind DegradeReason = "unsupported_kind" // 不支持的 Go 类型，降级为文本
	ReasonBadNumber       DegradeReason = "bad_number"       // NaN/Inf 等无法表示的数值，降级为文本
	ReasonTypeCoerceFail  DegradeReason = "type_coerce_fail" // 强制列类型与值不兼容，降级为文本
)

// DegradeEvent 记录一次降级，便于调用方审计。
type DegradeEvent struct {
	Sheet  string
	Row    int64 // 数据行序号（0 起，不含表头）
	Column int
	Reason DegradeReason
}

// SheetResult 单个工作表的导出结果。
type SheetResult struct {
	Name     string
	Rows     int64
	CSVPath  string // 该工作表对应的 CSV 文件路径
	Degrades []DegradeEvent
}

// Result 一次成功导出的结果。
type Result struct {
	XLSXPath string
	Sheets   []SheetResult // 顺序与工作表声明顺序一致
	Duration time.Duration
}
