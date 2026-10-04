package export

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// xlsxSheetWriter 流式写出单个工作表的 sheet XML 临时文件。
type xlsxSheetWriter struct {
	f  *os.File
	bw *bufio.Writer
}

const (
	sheetXMLHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`
	sheetXMLFooter = `</sheetData></worksheet>`
)

func newXLSXSheetWriter(path string) (*xlsxSheetWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	if _, err := io.WriteString(bw, sheetXMLHeader); err != nil {
		f.Close()
		return nil, err
	}
	return &xlsxSheetWriter{f: f, bw: bw}, nil
}

// writeRow 追加一行已分类的单元格，rowIndex 为 1 起的行号。
func (w *xlsxSheetWriter) writeRow(rowIndex int64, cells []cellValue) error {
	r := strconv.FormatInt(rowIndex, 10)
	if _, err := io.WriteString(w.bw, `<row r="`+r+`">`); err != nil {
		return err
	}
	for i, c := range cells {
		if c.typ == CellEmpty {
			continue // 空单元格省略，行为确定
		}
		writeXLSXCell(w.bw, colRef(i)+r, c)
	}
	_, err := io.WriteString(w.bw, `</row>`)
	return err
}

// flush 把缓冲数据落盘（由批处理逻辑按 BatchSize 调用）。
func (w *xlsxSheetWriter) flush() error {
	return w.bw.Flush()
}

func (w *xlsxSheetWriter) close() error {
	if _, err := io.WriteString(w.bw, sheetXMLFooter); err != nil {
		w.f.Close()
		return err
	}
	err := w.bw.Flush()
	cerr := w.f.Close()
	if err != nil {
		return err
	}
	return cerr
}

// 固定的 zip 时间戳，保证同样输入产出的字节完全一致（与运行时刻无关）。
var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

func zipWriteString(zw *zip.Writer, name, content string) error {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: zipEpoch}
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, content)
	return err
}

func zipCopyFile(zw *zip.Writer, name, srcPath string) error {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: zipEpoch}
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(w, src)
	return err
}

// contentTypesXML 按工作表数量生成 [Content_Types].xml。
func contentTypesXML(nSheets int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	b.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	b.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	b.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := 1; i <= nSheets; i++ {
		fmt.Fprintf(&b, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

const rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
	`</Relationships>`

// workbookXML 按声明顺序列出工作表；fullCalcOnLoad 保证公式在打开时重算。
func workbookXML(sheets []SheetDef) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" `)
	b.WriteString(`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`)
	b.WriteString(`<sheets>`)
	for i, s := range sheets {
		fmt.Fprintf(&b, `<sheet name="`)
		xmlEscape(&b, s.Name)
		fmt.Fprintf(&b, `" sheetId="%d" r:id="rId%d"/>`, i+1, i+1)
	}
	b.WriteString(`</sheets><calcPr fullCalcOnLoad="1"/></workbook>`)
	return b.String()
}

// workbookRelsXML 工作簿关系：前 nSheets 个为工作表，最后一个为样式。
func workbookRelsXML(nSheets int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 1; i <= nSheets; i++ {
		fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i)
	}
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, nSheets+1)
	b.WriteString(`</Relationships>`)
	return b.String()
}

// stylesXML 样式表：索引 0 为默认，索引 1 为日期（numFmt 164）。
const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
	`<numFmts count="1"><numFmt numFmtId="164" formatCode="yyyy-mm-dd hh:mm:ss"/></numFmts>` +
	`<fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts>` +
	`<fills count="1"><fill><patternFill patternType="none"/></fill></fills>` +
	`<borders count="1"><border/></borders>` +
	`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
	`<cellXfs count="2">` +
	`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>` +
	`<xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`</cellXfs>` +
	`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>` +
	`</styleSheet>`

// assembleXLSX 按工作表声明顺序把各 sheet XML 临时文件打包为 xlsx。
// zip 条目顺序与时间戳固定，同样输入产出字节级一致。
func assembleXLSX(outPath string, sheets []SheetDef, sheetXMLPaths []string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)

	if err := zipWriteString(zw, "[Content_Types].xml", contentTypesXML(len(sheets))); err != nil {
		goto fail
	}
	if err := zipWriteString(zw, "_rels/.rels", rootRelsXML); err != nil {
		goto fail
	}
	if err := zipWriteString(zw, "xl/workbook.xml", workbookXML(sheets)); err != nil {
		goto fail
	}
	if err := zipWriteString(zw, "xl/_rels/workbook.xml.rels", workbookRelsXML(len(sheets))); err != nil {
		goto fail
	}
	if err := zipWriteString(zw, "xl/styles.xml", stylesXML); err != nil {
		goto fail
	}
	for i := range sheets {
		name := fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)
		if err := zipCopyFile(zw, name, sheetXMLPaths[i]); err != nil {
			goto fail
		}
	}
	if err := zw.Close(); err != nil {
		goto fail
	}
	return f.Close()

fail:
	zw.Close()
	f.Close()
	os.Remove(outPath)
	return err
}
