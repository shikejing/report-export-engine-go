# report-export-engine-go

服务端报表导出引擎：把持续到达的数据行流式导出为 **XLSX 电子表格**，并在同一次导出中同时产出 **CSV（逗号分隔文本）**。两种格式共享同一套取值与转义流水线，结果逐条一致。

- **流式、有界内存**：数据先经类型规范化后写入磁盘假脱机（spool）文件，导出时再流式拼装；峰值内存只与批大小相关，不随总行数增长。
- **确定性**：工作表顺序、列顺序、空表/空行行为、重复值处理、ZIP 成员顺序与数字格式全部由报表定义和输入顺序决定，与缓存容量、map 遍历、并发调度无关。
- **并发导出**：多个工作表可并发渲染；合并严格按定义顺序进行，产物与单线程串行导出字节一致。
- **可取消、可重跑**：尊重 `context` 取消/超时与配置超时；所有产物先写隐藏的 `*.partial` 临时文件，成功 fsync 后才原子改名。取消或失败时不留任何可见半成品；重跑结果与一次成功完全一致。
- **坏数据不炸全局**：无法识别的值按固定策略降级为文本并附带可区分的原因码；单个工作表的写入失败只让该表降级为仅表头，不影响其它工作表。

## 安装与快速开始

```go
import re "github.com/shikejing/report-export-engine-go"

eng, err := re.NewEngine(re.Config{
    BatchSize:   1000,
    MaxSheets:   64,
    Concurrency: 4,
    Timeout:     30 * time.Second,
})

h, err := eng.NewReport(re.Report{Sheets: []re.SheetConfig{
    {Name: "orders", Columns: []re.Column{{Name: "id"}, {Name: "amount"}, {Name: "paid"}, {Name: "at"}},
}})
defer h.Close()

// 数据可以按任意批次、任意节奏持续写入
w := h.Writer(0)
for row := range rows {
    if err := w.WriteRow(ctx, re.Row{row.ID, row.Amount, row.Paid, re.AsDate(row.At)}); err != nil {
        return err // ctx 取消或 spool I/O 失败
    }
}

res, err := h.Export(ctx, "/var/reports/out", re.ExportOptions{Name: "orders-2024"})
// res.XLSXPath            -> /var/reports/out/orders-2024.xlsx
// res.CSVPaths["orders"]  -> /var/reports/out/orders-2024-01-orders.csv
// res.Warnings            -> 所有降级告警（稳定原因码 + 位置）
```

## 输出文件

| 格式 | 路径 | 说明 |
| --- | --- | --- |
| XLSX | `<outDir>/<name>.xlsx` | 一个工作簿，含全部工作表 |
| CSV | `<outDir>/<name>-<NN>-<sheetName>.csv` | 每个工作表一个文件，`NN` 为从 1 开始的工作表序号，名称中的非 `[A-Za-z0-9._-]` 字符替换为 `_` |

`ExportOptions.Formats` 为空时同时导出两种格式；也可显式传 `[]Format{FormatXLSX}` 或 `[]Format{FormatCSV}`。`Name` 缺省为 `report`。

## 单元格类型识别与显示规则

原始值在**写入时只做一次**规范化（`inferCell`），规范化结果落 spool；XLSX 与 CSV 读取的都是同一个规范值，因此二者永远不可能因格式层差异产生分歧。

| 输入 Go 值 | 类型 | XLSX 存储 | 双格式统一显示文本 |
| --- | --- | --- | --- |
| `nil`、类型化 nil 指针 | 空白 | 自闭合空单元格 | 空 |
| `string` | 文本 | inlineStr | 原文（**不做数字/公式猜测**，`"007"` 仍是文本 `"007"`，`"=1+1"` 仍是文本） |
| `bool` | 布尔 | `t="b"`，`0/1` | `TRUE` / `FALSE` |
| `int/int8/.../uint.../float32/float64` | 数值 | `<v>` 数字 | 十进制最短规范文本 |
| `encoding/json.Number` | 数值 | 同上 | 解析为 float64 后同一路径规范化 |
| `time.Time` | 日期时间 | 日期序列值 + 样式 `yyyy-mm-dd hh:mm:ss` | `2006-01-02 15:04:05`（UTC） |
| `[]byte` | 文本 | inlineStr | 按 UTF-8 字符串 |
| 指针 | — | 先解引用，nil 指针为空白 | 同被指向值 |
| 其它（struct/map/slice/func/chan…） | **降级文本** | inlineStr | `fmt.Sprint(v)`，告警 `UNSUPPORTED_TYPE` |

显式包装器：

- `AsNumber(v)`：强制数值；非数字类型降级文本，原因码 `NUMBER_INVALID`。
- `AsBool(b)`、`AsText(s)`：显式标注（布尔/字符串本就直接识别，包装器用于表意）。
- `AsDate(t)`：日期时间；`AsDateOnly(t)`：仅日期（序列值规整为当日零点整数，样式 `yyyy-mm-dd`，显示 `2006-01-02`）。
- `AsFormula(expr, cached)`：公式单元格。`expr` 不含前导 `=`，写入 `<f>`；`cached` 为缓存结果（可 nil，取值走同一张规则表）。CSV 无法承载公式，显示缓存值；缓存为 nil 时显示空。缓存值无法识别时原因码 `FORMULA_CACHED_INVALID`。
- 自定义类型可实现 `CellValue` 接口（`AsCell() (Cell, error)`）自行提供规范单元格；返回错误时降级文本，原因码 `UNSUPPORTED_TYPE`。

### 数值规范化（重复值稳定）

所有数值先转为 float64 再以 `strconv.FormatFloat(f, 'g', -1, 64)` 输出唯一规范文本：`1`、`1.0`、`int(1)`、`json.Number("1")` 的结果完全相同；与到达顺序、缓存容量无关。`NaN`、`+Inf`、`-Inf` 在电子表格中没有对应表示，降级为文本（`NaN`/`+Inf`/`-Inf`）并告警 `NUMBER_OUT_OF_RANGE`。

### 日期

采用 Excel 1900 日期系统（序列值 1 = 1900-01-01，含历史性的 1900 虚日），序列值是绝对时刻的纯函数。显示统一按 **UTC** 格式化，避免随运行机器时区变化。

### 非法字符降级

XML 1.0 不允许的控制字符（除 Tab/LF/CR 外的 `< 0x20` 字符）统一替换为 U+FFFD。该替换发生在 XLSX 与 CSV 共享的显示路径上，两格式表现一致，且不会导致导出失败。

### 降级原因码（`Warning.Reason`）

| 原因码 | 触发条件 |
| --- | --- |
| `UNSUPPORTED_TYPE` | Go 类型无法识别（或 `CellValue.AsCell` 返回错误），已降级为文本 |
| `NUMBER_INVALID` | `AsNumber` 作用于非数字值 |
| `NUMBER_OUT_OF_RANGE` | `NaN` / `+Inf` / `-Inf` |
| `TOO_MANY_VALUES` | 一行的值数超过列数，多余值被丢弃 |
| `FORMULA_CACHED_INVALID` | 公式缓存值无法识别，缓存结果降级为文本 |
| `SHEET_FAILED` | 该工作表写入阶段发生 spool I/O 失败，导出时该表仅输出表头 |

`Warning` 带 `Sheet`、`Row`（从 1 开始的数据行号）、`Column`（列字母，如 `B`）、`Detail`。告警始终按**工作表顺序、再按行追加顺序**收集，重跑与并发都不会改变其顺序。

## 行列与空表/空行语义

- 工作表必须非空且数量不超过 `MaxSheets`；名称非空、不得包含 `\ / ? * [ ] :`、不得重复；每个工作表至少定义一列，列名非空。违反时 `NewReport` 返回明确错误。
- 列数固定。短行右侧以空白补齐；长行截断多余值并对该行告警 `TOO_MANY_VALUES`。
- **空工作表**（零数据行）仍出现在工作簿中，XLSX 含仅表头的 sheet，CSV 文件仅含表头行。
- **空行**（空切片或全空）保留行位：XLSX 输出空白 `<row>`，CSV 输出空记录。

## 转义语义（两格式一致）

- **CSV**：RFC 4180。字段含 `"`、`,`、CR、LF 时整体加双引号；引号双写；记录以 CRLF 结尾。
- **XLSX**：文本走 inlineStr，`& < >` 转义为实体，Tab/LF/CR 输出为 `&#9; &#10; &#13;`，属性中 `"` 转义为 `&quot;`；不使用共享字符串表（因此重复值无去重状态、无顺序依赖）。
- 两格式的字段文本都取自同一个 `Cell.displayText()`，取值与需要转义的字符完全一致。

## 并发模型

- 每个工作表一个独立 spool，`WriteRow` 可由不同 goroutine 调用（每个工作表内部串行化）。
- 导出时 CSV 各表任务经信号量限流（`Concurrency`）并发执行；XLSX 最后合并，**始终按 `Report.Sheets` 顺序**回放各 spool。
- 合并出的 XLSX 与串行导出字节一致（固定 ZIP 成员顺序/命名、固定 XML 形状、固定零时间戳、Deflate）。

## 取消、超时与清理

- `Export` 尊重传入 `ctx`；另可用 `Config.Timeout` 或 `ExportOptions.Timeout`（后者优先）设置整体超时。超时/取消返回包装了 `ErrExportCanceled` 的错误。
- 产物写 `<path>.partial` → fsync → rename，并对目录 fsync；失败或取消时删除本次调用产生的所有 partial 与最终文件，输出目录中不会出现半成品。
- Spool 位于 `Config.TempDir` 下每次导出独立的临时目录，在 `ReportHandle.Close()` 时清理（幂等，可在失败/取消后调用）。spool 在 `Close` 前保留，因此**失败后直接再次调用 `Export` 即可重跑**，结果与一次成功完全一致。

## 配置项

| 字段 | 默认 | 校验 | 越界行为 |
| --- | --- | --- | --- |
| `BatchSize` | `1000` | 必须 > 0（0 取默认） | 负数报 `ErrInvalidConfig` |
| `MaxSheets` | `64` | 必须 > 0（0 取默认） | 负数报错；`NewReport` 超限报 `ErrTooManySheets` |
| `Concurrency` | `runtime.NumCPU()` | 必须 > 0（0 取默认） | 负数报错；`1` 即完全串行 |
| `Timeout` | `0`（不超时） | 必须 >= 0 | 负数报错；超时返回 `ErrExportCanceled` |
| `TempDir` | `os.TempDir()` | 必须是已存在可写目录 | 否则报 `ErrInvalidConfig` |

非法配置在 `NewEngine` 即报错，不会创建任何临时状态。

## 测试

```
go test ./...            # 全量（含 40 万行有界内存用例）
go test -short ./...     # 跳过大行数用例
go test -race ./...      # 竞态检测
```

覆盖：大行数峰值内存、重复值规范化、多工作表、并发写入/并发导出与串行字节一致、CSV/XLSX 取值与转义一致、空表空行、坏数据隔离、取消与超时清理、失败重跑、配置与报表校验、spool 往返。
