# report-export-engine-go

流式报表导出引擎：把持续到达的数据行同时导出为 **XLSX 电子表格** 与 **CSV 文本**。
两种格式共用同一套取值与转义语义，峰值内存不随总行数增长，支持取消、超时与并发导出。

## 快速开始

```go
eng, err := export.NewEngine(export.Config{
    BatchSize:   1024,          // 每表内存缓冲行数，达到即落盘
    MaxSheets:   16,            // 单次导出工作表上限
    Concurrency: 4,             // 并发写行的工作表上限
    TempDir:     "/var/tmp",    // 临时文件目录
})
if err != nil { /* 配置非法，err 指明具体字段 */ }

x, err := eng.Export(ctx, []export.SheetDef{
    {Name: "orders", Columns: []export.Column{
        {Name: "id"}, {Name: "amount"}, {Name: "paid"},
        {Name: "created_at"}, {Name: "note"},
    }},
})
if err != nil { /* 表定义非法（重名/无列/超上限） */ }

w, _ := x.SheetWriter("orders")
for row := range rowStream {            // 数据行持续到达
    if err := w.WriteRow(ctx, row); err != nil {
        x.Abort()                       // 取消：清理全部临时文件
        return err
    }
}
w.Close()

res, err := x.Commit(ctx, "out/report.xlsx", "out/csv")
// 成功：report.xlsx + out/csv/01_orders.csv 原子产出
// 失败/取消：不留下任何可见半成品文件
```

## 导出语义（确定性保证）

1. **流式与内存**：行按 `BatchSize` 批量落盘到临时文件，峰值内存只与
   `BatchSize × 并发表数` 相关，与总行数无关。实测 200 万行导出峰值 RSS 约 13MB。
2. **批次与到达顺序无关**：`BatchSize` 只影响落盘时机，不影响任何字节内容；
   同一输入按任意批次切分，产物逐字节一致。
3. **重复值与缓存无关**：XLSX 使用行内字符串（inline string），
   **不维护共享字符串表/去重缓存**，因此结果不依赖缓存容量或遍历顺序。
4. **多工作表**：工作表顺序 = 声明顺序；列顺序 = 定义顺序。
   空表产出仅含表头的确定结果；空行（全 nil）产出确定的空字段行。
   单表内的坏数据只降级该单元格并记录原因，不影响其它工作表。
5. **并发一致**：多个工作表可并发写行；每个工作表写独立的临时文件，
   最终按声明顺序合并，因此并发产物与单线程按相同顺序导出**逐字节一致**。
   `Concurrency` 只限制并发度，不改变任何顺序。
6. **取消与超时**：通过 `context.Context` 取消/超时。`Abort` 或失败的
   `Commit` 会清理全部临时文件；最终文件先写临时副本再原子 rename，
   中途失败不留可见半成品。重跑（相同输入）与一次成功导出逐字节一致。
7. **重跑覆盖**：输出路径已存在时会被覆盖，重跑结果与首次成功一致。

## 单元格类型识别规则

| Go 值 | 识别类型 | 规范文本（CSV 与 XLSX 共用） |
|---|---|---|
| `nil` | empty | `""`（XLSX 中省略该单元格） |
| `string`（不以 `=` 开头） | text | 原样 |
| `string`（以 `=` 开头） | formula | 原样（XLSX 写为 `<f>`，打开时重算） |
| `bool` | bool | `true` / `false` |
| 所有整数类型 | number | 十进制 |
| `float32/64` | number | `strconv.FormatFloat(f,'g',-1,64)` |
| `time.Time` | date | UTC，`2006-01-02T15:04:05Z`（XLSX 中为序列值 + `yyyy-mm-dd hh:mm:ss` 格式） |
| 其它类型 | **降级**为 text | `fmt.Sprint(v)`，原因 `unsupported_kind` |

**强制列类型**（`Column.Type` 非 `CellEmpty`）：按上表先识别，再向列类型转换；
无法兼容时**降级为文本**，原因 `type_coerce_fail`。`NaN`/`Inf` 降级为文本，
原因 `bad_number`。所有降级都记录在 `Result.Sheets[i].Degrades`，
导出**不会**因坏数据失败。

**转义语义**：CSV 遵循 RFC 4180（`encoding/csv`）；XLSX 遵循 XML 转义。
两种格式的取值都来自同一个 `formatText`，因此逐条一致。

## 配置项

| 字段 | 默认 | 含义 | 越界行为 |
|---|---|---|---|
| `BatchSize` | 1024 | 每表内存缓冲行数，达到即落盘 | `<=0` 用默认值；`<0` 报错 |
| `MaxSheets` | 64 | 单次导出工作表数上限 | 超出时 `Export` 返回 `ErrTooManySheets` |
| `Concurrency` | 4 | 并发写行的工作表上限 | 只限流，不影响结果 |
| `TempDir` | 系统临时目录 | 临时文件目录 | 必须可写 |
| `MaxRowsSheet` | 0（不限） | 单表最大数据行数 | 超出时 `WriteRow` 返回 `ErrTooManyRows` |

非法配置（负数）在 `NewEngine` 时返回 `ErrInvalidConfig` 并指明字段。

## 产物布局

```
out/report.xlsx          # 所有工作表，按声明顺序
out/csv/01_orders.csv    # 每表一个 CSV：序号_名称.csv
out/csv/02_summary.csv
```

## 测试

```sh
go test ./export/          # 单元 + 集成测试
go test ./export/ -race    # 并发正确性
```

覆盖：20 万行大导出、批切分不变性、重复值确定性、多工作表顺序与空表空行、
并发与单线程逐字节一致、取消与清理、重跑一致、配置与边界校验、CSV/XLSX 取值一致。
