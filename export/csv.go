package export

import (
	"bufio"
	"encoding/csv"
	"os"
)

// csvSheetWriter 流式写出单个工作表的 CSV 临时文件。
// 转义语义由 encoding/csv 保证（RFC 4180），与 XLSX 共用 formatText
// 产出的规范文本，因此两种格式取值一致。
type csvSheetWriter struct {
	f  *os.File
	bw *bufio.Writer
	cw *csv.Writer
}

func newCSVSheetWriter(path string) (*csvSheetWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	cw := csv.NewWriter(bw)
	return &csvSheetWriter{f: f, bw: bw, cw: cw}, nil
}

// writeRow 追加一行（已格式化的文本单元格）。
func (w *csvSheetWriter) writeRow(cells []string) error {
	return w.cw.Write(cells)
}

// flush 把缓冲数据落盘（由批处理逻辑按 BatchSize 调用）。
func (w *csvSheetWriter) flush() error {
	w.cw.Flush()
	if err := w.cw.Error(); err != nil {
		return err
	}
	return w.bw.Flush()
}

func (w *csvSheetWriter) close() error {
	err := w.flush()
	cerr := w.f.Close()
	if err != nil {
		return err
	}
	return cerr
}
