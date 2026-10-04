package reportexport

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Reason is a stable machine-readable degradation reason code. Warnings with
// different causes always carry different codes.
type Reason string

const (
	// ReasonUnsupportedType: value's Go type is not coercible; rendered as
	// text via fmt.Sprint and flagged.
	ReasonUnsupportedType Reason = "UNSUPPORTED_TYPE"
	// ReasonNumberInvalid: AsNumber received a non-numeric value.
	ReasonNumberInvalid Reason = "NUMBER_INVALID"
	// ReasonNumberOutOfRange: numeric value is NaN or +/-Inf, which has no
	// spreadsheet representation; rendered as text.
	ReasonNumberOutOfRange Reason = "NUMBER_OUT_OF_RANGE"
	// ReasonTooManyValues: a row carried more values than columns; extras
	// were dropped.
	ReasonTooManyValues Reason = "TOO_MANY_VALUES"
	// ReasonFormulaCachedInvalid: a formula's cached value was not
	// coercible; the cached result degrades to text.
	ReasonFormulaCachedInvalid Reason = "FORMULA_CACHED_INVALID"
	// ReasonSheetFailed: the worksheet writer failed mid-stream; the sheet
	// is emitted empty and flagged; other sheets are unaffected.
	ReasonSheetFailed Reason = "SHEET_FAILED"
)

// Date layouts. The XLSX styles use the same patterns (yyyy-mm-dd and
// yyyy-mm-dd hh:mm:ss), so a date cell looks identical in both formats.
const (
	layoutDate     = "2006-01-02"
	layoutDateTime = "2006-01-02 15:04:05"
)

// AsText marks v as a text cell. A plain string already infers to text; this
// exists for interface symmetry and to make intent explicit.
func AsText(v string) any { return v }

// AsNumber marks v as a numeric cell. v must be a built-in numeric kind or an
// encoding/json.Number; otherwise the cell degrades to text with
// ReasonNumberInvalid. NaN/+Inf/-Inf degrade with ReasonNumberOutOfRange.
func AsNumber(v any) any { return forcedNumber{v} }

// AsBool marks v as a boolean cell.
func AsBool(v bool) any { return v }

// AsDate marks t as a date-time cell displayed as "YYYY-MM-DD HH:MM:SS".
func AsDate(t time.Time) any { return t }

// AsDateOnly marks t as a date cell displayed as "YYYY-MM-DD".
func AsDateOnly(t time.Time) any { return dateOnly{t} }

// AsFormula defines a formula cell with a cached result.
//
// expression excludes the leading "=" and is written verbatim between <f>
// tags. cached may be nil or any coercible value; spreadsheets that display
// cached results show it, and CSV output (which cannot carry formulas) shows
// it as the cell value. A nil cached value renders as blank.
func AsFormula(expression string, cached any) any { return formulaVal{expression, cached} }

// forcedNumber forces numeric inference for AsNumber.
type forcedNumber struct{ v any }

// dateOnly forces date-only rendering.
type dateOnly struct{ t time.Time }

type formulaVal struct {
	expr   string
	cached any
}

// CellValue is implemented by caller types that carry their own canonical
// representation. The returned Cell is used directly; a non-nil error causes
// text degradation with ReasonUnsupportedType and the error text as detail.
type CellValue interface {
	AsCell() (Cell, error)
}

// inferCell coerces one raw value into a canonical Cell. See doc.go for the
// full, fixed rule table. The returned reason/detail are empty when no
// degradation occurred.
func inferCell(v any) (Cell, Reason, string) {
	if v == nil {
		return Cell{Kind: KindBlank}, "", ""
	}
	switch x := v.(type) {
	case string:
		return Cell{Kind: KindText, Text: x}, "", ""
	case bool:
		return Cell{Kind: KindBool, Bool: x}, "", ""
	case time.Time:
		return dateCell(x, false), "", ""
	case dateOnly:
		return dateCell(x.t, true), "", ""
	case []byte:
		return Cell{Kind: KindText, Text: string(x)}, "", ""
	case json.Number:
		return numberFromFloat(string(x), true)
	case forcedNumber:
		return forceNumber(x.v)
	case formulaVal:
		return formulaCell(x)
	case CellValue:
		c, err := x.AsCell()
		if err != nil {
			return Cell{Kind: KindText, Text: fmt.Sprint(v)}, ReasonUnsupportedType, err.Error()
		}
		return c, "", ""
	}

	// Built-in numeric kinds.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return Cell{Kind: KindNumber, Number: strconv.FormatInt(rv.Int(), 10)}, "", ""
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return Cell{Kind: KindNumber, Number: strconv.FormatUint(rv.Uint(), 10)}, "", ""
	case reflect.Float32, reflect.Float64:
		return numberFromFloat(strconv.FormatFloat(rv.Float(), 'g', -1, 64), false)
	case reflect.Pointer:
		if rv.IsNil() {
			return Cell{Kind: KindBlank}, "", ""
		}
		return inferCell(rv.Elem().Interface())
	}

	// Defined strategy for unrecognized values: degrade to text, never fail
	// the export; flag with a distinguishable reason.
	return Cell{Kind: KindText, Text: fmt.Sprint(v)}, ReasonUnsupportedType,
		"unsupported Go type: " + rv.Type().String()
}

// numberFromFloat turns a shortest-round-trip decimal token into a number
// cell, degrading NaN/Inf to text.
func numberFromFloat(token string, fromJSON bool) (Cell, Reason, string) {
	f, err := strconv.ParseFloat(token, 64)
	if err != nil {
		return Cell{Kind: KindText, Text: token}, ReasonNumberInvalid, "not a number: " + token
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Cell{Kind: KindText, Text: token}, ReasonNumberOutOfRange, token
	}
	// Normalize so float32-origin values and json tokens share one canonical
	// spelling: re-emit through FormatFloat after ParseFloat. Integer-valued
	// floats keep a ".0"? No — 'g' emits "1" for 1.0. That canonicalizes
	// 1.0 and 1 to identical output, which is the intended duplicate-stable
	// behavior.
	return Cell{Kind: KindNumber, Number: strconv.FormatFloat(f, 'g', -1, 64)}, "", ""
}

// forceNumber implements AsNumber: only numeric kinds (and json.Number) are
// accepted; bool/string/etc. degrade with ReasonNumberInvalid.
func forceNumber(v any) (Cell, Reason, string) {
	if v == nil {
		return Cell{Kind: KindBlank}, "", ""
	}
	switch x := v.(type) {
	case json.Number:
		return numberFromFloat(string(x), true)
	case float32:
		return numberFromFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), false)
	case float64:
		return numberFromFloat(strconv.FormatFloat(x, 'g', -1, 64), false)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return Cell{Kind: KindNumber, Number: strconv.FormatInt(rv.Int(), 10)}, "", ""
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return Cell{Kind: KindNumber, Number: strconv.FormatUint(rv.Uint(), 10)}, "", ""
	case reflect.Pointer:
		if rv.IsNil() {
			return Cell{Kind: KindBlank}, "", ""
		}
		return forceNumber(rv.Elem().Interface())
	}
	return Cell{Kind: KindText, Text: fmt.Sprint(v)}, ReasonNumberInvalid,
		"AsNumber applied to non-numeric type: " + rv.Type().String()
}

// excelSerial converts an instant to an Excel serial number under the 1900
// date system (serial 1 == 1900-01-01; the fictional 1900-02-29 is included
// for historical compatibility). The conversion is a pure function of the
// absolute instant, so it is independent of location.
func excelSerial(t time.Time) float64 {
	const day = 86400
	u := t.Unix()
	days := int64(u / day)
	rem := u % day
	if rem < 0 {
		rem += day
		days--
	}
	frac := float64(rem) / float64(day)
	// Add sub-second portion that Unix() truncates.
	frac += float64(t.Nanosecond()) / float64(day) / 1e9
	return float64(days) + 25569.0 + frac
}

func dateCell(t time.Time, dateOnly bool) Cell {
	var (
		layout string
		dv     dateVal
	)
	if dateOnly {
		layout = layoutDate
		// Normalize to midnight in the value's own location so the serial is
		// an exact integer: a date-only cell carries no hidden time fraction,
		// keeping its stored value consistent with its displayed date.
		y, m, d := t.Date()
		t = time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	} else {
		layout = layoutDateTime
	}
	dv.Serial = excelSerial(t)
	dv.Date = dateOnly
	return Cell{Kind: KindDate, Text: t.Format(layout), Date: dv}
}

func formulaCell(f formulaVal) (Cell, Reason, string) {
	c := Cell{Kind: KindFormula, Formula: f.expr}
	if f.cached != nil {
		cached, reason, detail := inferCell(f.cached)
		if reason != "" {
			reason = ReasonFormulaCachedInvalid
		}
		c.Cached = &cached
		return c, reason, detail
	}
	return c, "", ""
}

// displayText renders the canonical display value shared by CSV and by XLSX
// number/boolean/date text. It never consults the original raw value, so the
// two formats cannot diverge due to formatting or escaping.
func (c Cell) displayText() string {
	switch c.Kind {
	case KindBlank:
		return ""
	case KindText:
		return sanitizeForOutput(c.Text)
	case KindNumber:
		return c.Number
	case KindBool:
		if c.Bool {
			return "TRUE"
		}
		return "FALSE"
	case KindDate:
		return c.Text
	case KindFormula:
		if c.Cached == nil {
			return ""
		}
		return c.Cached.displayText()
	default:
		return ""
	}
}

// colLetter converts a 0-based column index to the A1 letter (0 -> A).
func colLetter(n int) string {
	if n < 0 {
		return ""
	}
	var b strings.Builder
	for n >= 0 {
		b.WriteByte(byte('A' + n%26))
		n = n/26 - 1
	}
	r := []byte(b.String())
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}
