package reportexport

import (
	"archive/zip"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// XLSX assembly notes
//
// The workbook is produced with archive/zip straight into the final file's
// hidden ".partial" sibling, so no full workbook is ever held in memory. All
// string cells use inline strings (t="inlineStr") rather than a shared string
// table: duplicate values cost no deduplication state, and output never
// depends on cache capacity or the order in which values first appeared.
//
// Archive members are written in one fixed order, positional names
// (sheet1.xml, sheet2.xml, ...) and a fixed XML shape; the deflated bytes for
// identical input are identical across runs and across serial/concurrent
// sheet export (merge always iterates Report.Sheets in definition order).

const (
	styleDefault  = 0 // cellXfs[0]
	styleDateOnly = 1 // cellXfs[1] yyyy-mm-dd
	styleDateTime = 2 // cellXfs[2] yyyy-mm-dd hh:mm:ss
	styleHeader   = 3 // cellXfs[3] bold header text
)

// escapeXMLText writes XML 1.0 text content. The only control characters
// legal in XML 1.0 (tab, LF, CR) are emitted as numeric entities so their
// bytes survive parsing unchanged.
func escapeXMLText(w io.Writer, s string) error {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '&':
			io.WriteString(w, "&amp;")
		case '<':
			io.WriteString(w, "&lt;")
		case '>':
			io.WriteString(w, "&gt;")
		case '\t':
			io.WriteString(w, "&#9;")
		case '\n':
			io.WriteString(w, "&#10;")
		case '\r':
			io.WriteString(w, "&#13;")
		default:
			if !validXMLRune(r) {
				io.WriteString(w, "\ufffd")
			} else {
				io.WriteString(w, s[i:i+size])
			}
		}
		i += size
	}
	return nil
}

func escapeXMLAttr(w io.Writer, s string) error {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '&':
			io.WriteString(w, "&amp;")
		case '<':
			io.WriteString(w, "&lt;")
		case '>':
			io.WriteString(w, "&gt;")
		case '"':
			io.WriteString(w, "&quot;")
		case '\t':
			io.WriteString(w, "&#9;")
		case '\n':
			io.WriteString(w, "&#10;")
		case '\r':
			io.WriteString(w, "&#13;")
		default:
			if !validXMLRune(r) {
				io.WriteString(w, "\ufffd")
			} else {
				io.WriteString(w, s[i:i+size])
			}
		}
		i += size
	}
	return nil
}

func validXMLRune(r rune) bool {
	return r == 0x9 || r == 0xA || r == 0xD || r >= 0x20
}

// sanitizeForOutput replaces runes that are illegal in XML 1.0 (other than
// tab/LF/CR) with U+FFFD. It is applied to the shared display path so CSV and
// XLSX degrade identical input identically; callers never receive a hard
// error for such content.
func sanitizeForOutput(s string) string {
	if !containsInvalidRune(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' || r == '\n' || r == '\r' || r >= 0x20 {
			b.WriteRune(r)
		} else {
			b.WriteRune('�')
		}
	}
	return b.String()
}

func containsInvalidRune(s string) bool {
	for _, r := range s {
		if r != '\t' && r != '\n' && r != '\r' && r < 0x20 {
			return true
		}
	}
	return false
}

func formatSerial(f float64) string {
	s := strconv.FormatFloat(f, 'f', 9, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// xlsxWriter emits one worksheet's sheetData XML into a writer.
type xlsxWriter struct {
	w *bufio.Writer
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"

func (x *xlsxWriter) writePreamble() error {
	_, err := io.WriteString(x.w, xmlHeader)
	return err
}

func (x *xlsxWriter) writeHeader(cols []Column) error {
	if _, err := io.WriteString(x.w, `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`); err != nil {
		return err
	}
	return x.writeColumnCells(1, styleHeader, func(i int) string { return sanitizeForOutput(cols[i].Name) }, len(cols), "inlineStr")
}

// writeColumnCells writes one row of text cells.
func (x *xlsxWriter) writeColumnCells(rowIdx, style int, val func(int) string, n int, t string) error {
	if _, err := fmt.Fprintf(x.w, `<row r="%d">`, rowIdx); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		ref := colLetter(i) + strconv.Itoa(rowIdx)
		if _, err := io.WriteString(x.w, `<c r="`+ref+`" s="`+strconv.Itoa(style)+`" t="inlineStr"><is><t xml:space="preserve">`); err != nil {
			return err
		}
		if err := escapeXMLText(x.w, val(i)); err != nil {
			return err
		}
		if _, err := io.WriteString(x.w, `</t></is></c>`); err != nil {
			return err
		}
	}
	_, err := io.WriteString(x.w, `</row>`)
	return err
}

// writeRow emits one data row. cells are already padded/truncated to column
// count. Blank cells are emitted with an explicit reference so column
// positions stay fixed.
func (x *xlsxWriter) writeRow(rowIdx int, cells []Cell) error {
	if _, err := fmt.Fprintf(x.w, `<row r="%d">`, rowIdx); err != nil {
		return err
	}
	for i, c := range cells {
		ref := colLetter(i) + strconv.Itoa(rowIdx)
		if err := x.writeCell(ref, c); err != nil {
			return err
		}
	}
	_, err := io.WriteString(x.w, `</row>`)
	return err
}

func (x *xlsxWriter) writeCell(ref string, c Cell) error {
	switch c.Kind {
	case KindBlank:
		_, err := io.WriteString(x.w, `<c r="`+ref+`"/>`)
		return err
	case KindText:
		if _, err := io.WriteString(x.w, `<c r="`+ref+`" t="inlineStr"><is><t xml:space="preserve">`); err != nil {
			return err
		}
		if err := escapeXMLText(x.w, sanitizeForOutput(c.Text)); err != nil {
			return err
		}
		_, err := io.WriteString(x.w, `</t></is></c>`)
		return err
	case KindNumber:
		if _, err := io.WriteString(x.w, `<c r="`+ref+`"><v>`); err != nil {
			return err
		}
		if _, err := io.WriteString(x.w, c.Number); err != nil {
			return err
		}
		_, err := io.WriteString(x.w, `</v></c>`)
		return err
	case KindBool:
		v := "0"
		if c.Bool {
			v = "1"
		}
		_, err := io.WriteString(x.w, `<c r="`+ref+`" t="b"><v>`+v+`</v></c>`)
		return err
	case KindDate:
		style := styleDateOnly
		if !c.Date.Date {
			style = styleDateTime
		}
		if _, err := io.WriteString(x.w, `<c r="`+ref+`" s="`+strconv.Itoa(style)+`"><v>`); err != nil {
			return err
		}
		if _, err := io.WriteString(x.w, formatSerial(c.Date.Serial)); err != nil {
			return err
		}
		_, err := io.WriteString(x.w, `</v></c>`)
		return err
	case KindFormula:
		return x.writeFormulaCell(ref, c)
	}
	return nil
}

func (x *xlsxWriter) writeFormulaCell(ref string, c Cell) error {
	open := `<c r="` + ref + `"`
	closeV := ""
	var value string
	switch {
	case c.Cached == nil || c.Cached.Kind == KindBlank:
		// no <v>: spreadsheet recomputes
	case c.Cached.Kind == KindBool:
		open += ` t="b"`
		if c.Cached.Bool {
			value = "1"
		} else {
			value = "0"
		}
		closeV = `<v>` + value + `</v>`
	case c.Cached.Kind == KindNumber:
		closeV = `<v>` + c.Cached.Number + `</v>`
	default:
		// Cached textual results use the formula-string type t="str".
		open += ` t="str"`
		var sb strings.Builder
		escapeXMLText(&sb, sanitizeForOutput(c.Cached.displayText()))
		closeV = `<v>` + sb.String() + `</v>`
	}
	if _, err := io.WriteString(x.w, open+`><f>`); err != nil {
		return err
	}
	if err := escapeXMLText(x.w, c.Formula); err != nil {
		return err
	}
	if _, err := io.WriteString(x.w, `</f>`+closeV+`</c>`); err != nil {
		return err
	}
	return nil
}

func (x *xlsxWriter) writeFooter() error {
	_, err := io.WriteString(x.w, `</sheetData></worksheet>`)
	return err
}

// ---- fixed package parts ----

func writeContentTypes(zw *zip.Writer, n int) error {
	w, err := zw.Create(`[Content_Types].xml`)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	b.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	b.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	b.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := 1; i <= n; i++ {
		b.WriteString(`<Override PartName="/xl/worksheets/sheet` + strconv.Itoa(i) + `.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`)
	}
	b.WriteString(`</Types>`)
	_, err = io.WriteString(w, b.String())
	return err
}

func writeRootRels(zw *zip.Writer) error {
	w, err := zw.Create(`_rels/.rels`)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, xmlHeader+
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>`+
		`</Relationships>`)
	return err
}

func writeWorkbook(zw *zip.Writer, rpt *Report) error {
	w, err := zw.Create(`xl/workbook.xml`)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i, sh := range rpt.Sheets {
		b.WriteString(`<sheet name="`)
		var sb strings.Builder
		escapeXMLAttr(&sb, sh.Name)
		b.WriteString(sb.String())
		b.WriteString(`" sheetId="` + strconv.Itoa(i+1) + `" r:id="rId` + strconv.Itoa(i+1) + `"/>`)
	}
	b.WriteString(`</sheets></workbook>`)
	_, err = io.WriteString(w, b.String())
	return err
}

func writeWorkbookRels(zw *zip.Writer, n int) error {
	w, err := zw.Create(`xl/_rels/workbook.xml.rels`)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 1; i <= n; i++ {
		b.WriteString(`<Relationship Id="rId` + strconv.Itoa(i) + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet` + strconv.Itoa(i) + `.xml"/>`)
	}
	b.WriteString(`</Relationships>`)
	_, err = io.WriteString(w, b.String())
	return err
}

func writeStyles(zw *zip.Writer) error {
	w, err := zw.Create(`xl/styles.xml`)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, xmlHeader+
		`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`+
		`<numFmts count="2"><numFmt numFmtId="164" formatCode="yyyy-mm-dd"/><numFmt numFmtId="165" formatCode="yyyy-mm-dd hh:mm:ss"/></numFmts>`+
		`<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>`+
		`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>`+
		`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>`+
		`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>`+
		`<cellXfs count="4">`+
		`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>`+
		`<xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>`+
		`<xf numFmtId="165" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>`+
		`<xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/>`+
		`</cellXfs>`+
		`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>`+
		`</styleSheet>`)
	return err
}

// streamSheet writes one worksheet member by replaying its spool in fixed
// sheet order. A failed sheet is emitted header-only (the failure was already
// recorded as a sheet-level warning when rows were written).
func streamSheet(ctx context.Context, zw *zip.Writer, idx int, job *sheetJob, batch int) error {
	w, err := zw.Create(`xl/worksheets/sheet` + strconv.Itoa(idx+1) + `.xml`)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 64*1024)
	xw := &xlsxWriter{w: bw}
	if err := xw.writePreamble(); err != nil {
		return err
	}
	if err := xw.writeHeader(job.cfg.Columns); err != nil {
		return err
	}
	rowIdx := 2
	n := 0
	if job.err == nil {
		err = job.spool.Rows(ctx, func(cells []Cell) error {
			if err := xw.writeRow(rowIdx, cells); err != nil {
				return err
			}
			rowIdx++
			n++
			if n%batch == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				return bw.Flush()
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := xw.writeFooter(); err != nil {
		return err
	}
	return bw.Flush()
}

// mergeXLSX assembles path atomically: fixed parts first, then worksheets in
// Report.Sheets order. On any error/cancel the .partial file is removed.
func mergeXLSX(ctx context.Context, path string, rpt *Report, sheets []*sheetJob, batch int) (err error) {
	tmp := path + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	abort := func() {
		f.Close()
		os.Remove(tmp)
	}
	zw := zip.NewWriter(f)
	check := func(e error) bool {
		if e != nil {
			zw.Close()
			abort()
			err = e
			return true
		}
		return false
	}
	if check(writeContentTypes(zw, len(sheets))) {
		return err
	}
	if err := ctx.Err(); err != nil {
		zw.Close()
		abort()
		return err
	}
	if check(writeRootRels(zw)) {
		return err
	}
	if check(writeWorkbook(zw, rpt)) {
		return err
	}
	if check(writeWorkbookRels(zw, len(sheets))) {
		return err
	}
	if check(writeStyles(zw)) {
		return err
	}
	for i, job := range sheets {
		if err := ctx.Err(); err != nil {
			zw.Close()
			abort()
			return err
		}
		if err := streamSheet(ctx, zw, i, job, batch); err != nil {
			zw.Close()
			abort()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		abort()
		return err
	}
	if err := f.Sync(); err != nil {
		abort()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
