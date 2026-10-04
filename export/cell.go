package export

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// cellValue 一行数据经类型识别后的单元格表示。
type cellValue struct {
	typ CellType
	raw any // string / int64 / float64 / bool / time.Time；empty 时为 nil
}

// dateLayout 是日期类型唯一的规范显示格式（UTC）。
const dateLayout = "2006-01-02T15:04:05Z"

// classify 按既定规则把任意 Go 值识别为单元格类型。
// 规则（与 docs 中文档一致）：
//   - nil                -> empty
//   - string             -> 以 "=" 开头为 formula，否则 text
//   - bool               -> bool
//   - 所有整数/浮点      -> number；NaN/Inf 降级为 text（ReasonBadNumber）
//   - time.Time          -> date（统一转为 UTC）
//   - 其它类型           -> 降级为 text（ReasonUnsupportedKind），值为 fmt.Sprint
//
// forced 非 CellEmpty 时按列声明强制转换，无法兼容时降级为 text
// （ReasonTypeCoerceFail）。返回降级原因（空串表示无降级）。
func classify(v any, forced CellType) (cellValue, DegradeReason) {
	cv, reason := infer(v)
	if forced == CellEmpty || forced == cv.typ || cv.typ == CellEmpty {
		return cv, reason
	}
	coerced, ok := coerce(cv, forced)
	if !ok {
		// 强制类型不兼容：降级为该值的文本表示，导出继续。
		return cellValue{typ: CellText, raw: formatText(cv)}, ReasonTypeCoerceFail
	}
	return coerced, reason
}

// infer 按 Go 类型自动识别。
func infer(v any) (cellValue, DegradeReason) {
	switch t := v.(type) {
	case nil:
		return cellValue{typ: CellEmpty}, ""
	case string:
		if strings.HasPrefix(t, "=") {
			return cellValue{typ: CellFormula, raw: t}, ""
		}
		return cellValue{typ: CellText, raw: t}, ""
	case bool:
		return cellValue{typ: CellBool, raw: t}, ""
	case time.Time:
		return cellValue{typ: CellDate, raw: t.UTC()}, ""
	case int:
		return cellValue{typ: CellNumber, raw: int64(t)}, ""
	case int8:
		return cellValue{typ: CellNumber, raw: int64(t)}, ""
	case int16:
		return cellValue{typ: CellNumber, raw: int64(t)}, ""
	case int32:
		return cellValue{typ: CellNumber, raw: int64(t)}, ""
	case int64:
		return cellValue{typ: CellNumber, raw: t}, ""
	case uint:
		return cellValue{typ: CellNumber, raw: uint64(t)}, ""
	case uint8:
		return cellValue{typ: CellNumber, raw: uint64(t)}, ""
	case uint16:
		return cellValue{typ: CellNumber, raw: uint64(t)}, ""
	case uint32:
		return cellValue{typ: CellNumber, raw: uint64(t)}, ""
	case uint64:
		return cellValue{typ: CellNumber, raw: t}, ""
	case float32:
		return classifyFloat(float64(t))
	case float64:
		return classifyFloat(t)
	default:
		// 不支持的类型：降级为文本，不让整次导出失败。
		return cellValue{typ: CellText, raw: fmt.Sprint(v)}, ReasonUnsupportedKind
	}
}

func classifyFloat(f float64) (cellValue, DegradeReason) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		// XLSX/CSV 均无法表示非有限数值：降级为文本。
		return cellValue{typ: CellText, raw: strconv.FormatFloat(f, 'g', -1, 64)}, ReasonBadNumber
	}
	return cellValue{typ: CellNumber, raw: f}, ""
}

// coerce 把已识别的值强制转换为列声明类型。
func coerce(cv cellValue, to CellType) (cellValue, bool) {
	switch to {
	case CellText:
		return cellValue{typ: CellText, raw: formatText(cv)}, true
	case CellNumber:
		if cv.typ == CellText {
			if f, err := strconv.ParseFloat(cv.raw.(string), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return cellValue{typ: CellNumber, raw: f}, true
			}
			return cellValue{}, false
		}
		return cellValue{}, false
	case CellBool:
		if cv.typ == CellText {
			switch cv.raw.(string) {
			case "true":
				return cellValue{typ: CellBool, raw: true}, true
			case "false":
				return cellValue{typ: CellBool, raw: false}, true
			}
		}
		return cellValue{}, false
	case CellDate:
		if cv.typ == CellText {
			if tm, err := time.Parse(dateLayout, cv.raw.(string)); err == nil {
				return cellValue{typ: CellDate, raw: tm.UTC()}, true
			}
		}
		return cellValue{}, false
	case CellFormula:
		if cv.typ == CellText {
			s := cv.raw.(string)
			if !strings.HasPrefix(s, "=") {
				s = "=" + s
			}
			return cellValue{typ: CellFormula, raw: s}, true
		}
		return cellValue{}, false
	}
	return cellValue{}, false
}

// formatText 返回单元格的规范文本表示，CSV 与 XLSX 共用，
// 保证两种格式在取值语义上完全一致。
func formatText(c cellValue) string {
	switch c.typ {
	case CellEmpty:
		return ""
	case CellText, CellFormula:
		return c.raw.(string)
	case CellBool:
		if c.raw.(bool) {
			return "true"
		}
		return "false"
	case CellDate:
		return c.raw.(time.Time).UTC().Format(dateLayout)
	case CellNumber:
		return formatNumber(c.raw)
	default:
		return ""
	}
}

// formatNumber 数值的唯一规范文本表示。
func formatNumber(v any) string {
	switch n := v.(type) {
	case int64:
		return strconv.FormatInt(n, 10)
	case uint64:
		return strconv.FormatUint(n, 10)
	case float64:
		return strconv.FormatFloat(n, 'g', -1, 64)
	default:
		return ""
	}
}

// excelSerial 把 UTC 时间转换为 Excel 日期序列值（1900 日期系统）。
func excelSerial(t time.Time) float64 {
	const unixToExcelEpochDays = 25569 // 1970-01-01 距 1899-12-30 的天数
	return float64(t.Unix())/86400 + unixToExcelEpochDays
}

// colRef 把 0 起的列序号转换为 Excel 列名（A、B、…、Z、AA…）。
func colRef(idx int) string {
	var buf [8]byte
	i := len(buf)
	n := idx + 1
	for n > 0 {
		n--
		i--
		buf[i] = byte('A' + n%26)
		n /= 26
	}
	return string(buf[i:])
}

// xmlEscape 把字符串按 XML 规则转义后写入 w。
func xmlEscape(w io.Writer, s string) {
	var buf strings.Builder
	buf.Grow(len(s) + 16)
	for _, r := range s {
		switch r {
		case '&':
			buf.WriteString("&amp;")
		case '<':
			buf.WriteString("&lt;")
		case '>':
			buf.WriteString("&gt;")
		case '"':
			buf.WriteString("&quot;")
		case '\'':
			buf.WriteString("&apos;")
		default:
			buf.WriteRune(r)
		}
	}
	io.WriteString(w, buf.String())
}

// writeXLSXCell 把单元格以 XLSX 行内 XML 形式写入 w。
// 空单元格不写出任何内容（XLSX 允许省略，行为确定）。
// 日期使用样式索引 1（在 styles.xml 中登记为 yyyy-mm-dd hh:mm:ss）。
func writeXLSXCell(w io.Writer, ref string, c cellValue) {
	switch c.typ {
	case CellEmpty:
		// 省略空单元格。
	case CellText:
		io.WriteString(w, `<c r="`+ref+`" t="inlineStr"><is><t xml:space="preserve">`)
		xmlEscape(w, c.raw.(string))
		io.WriteString(w, `</t></is></c>`)
	case CellNumber:
		io.WriteString(w, `<c r="`+ref+`"><v>`+formatNumber(c.raw)+`</v></c>`)
	case CellBool:
		v := "0"
		if c.raw.(bool) {
			v = "1"
		}
		io.WriteString(w, `<c r="`+ref+`" t="b"><v>`+v+`</v></c>`)
	case CellDate:
		serial := strconv.FormatFloat(excelSerial(c.raw.(time.Time)), 'f', -1, 64)
		io.WriteString(w, `<c r="`+ref+`" s="1"><v>`+serial+`</v></c>`)
	case CellFormula:
		f := strings.TrimPrefix(c.raw.(string), "=")
		io.WriteString(w, `<c r="`+ref+`"><f>`)
		xmlEscape(w, f)
		io.WriteString(w, `</f></c>`)
	}
}
