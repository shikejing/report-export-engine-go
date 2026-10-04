package export

import (
	"math"
	"testing"
	"time"
)

func TestClassifyBasicTypes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want CellType
		text string
	}{
		{"nil", nil, CellEmpty, ""},
		{"text", "hello", CellText, "hello"},
		{"empty string", "", CellText, ""},
		{"formula", "=SUM(A1:A2)", CellFormula, "=SUM(A1:A2)"},
		{"bool true", true, CellBool, "true"},
		{"bool false", false, CellBool, "false"},
		{"int", int(-3), CellNumber, "-3"},
		{"int64", int64(1 << 40), CellNumber, "1099511627776"},
		{"uint64", uint64(1 << 60), CellNumber, "1152921504606846976"},
		{"float", 1.5, CellNumber, "1.5"},
		{"float32", float32(0.25), CellNumber, "0.25"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cv, reason := classify(c.in, CellEmpty)
			if reason != "" {
				t.Fatalf("unexpected degrade: %s", reason)
			}
			if cv.typ != c.want {
				t.Fatalf("type = %v, want %v", cv.typ, c.want)
			}
			if got := formatText(cv); got != c.text {
				t.Fatalf("text = %q, want %q", got, c.text)
			}
		})
	}
}

func TestClassifyDateIsUTC(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	tm := time.Date(2024, 3, 1, 10, 30, 0, 0, loc)
	cv, reason := classify(tm, CellEmpty)
	if reason != "" || cv.typ != CellDate {
		t.Fatalf("type=%v reason=%v", cv.typ, reason)
	}
	if got := formatText(cv); got != "2024-03-01T02:30:00Z" {
		t.Fatalf("date text = %q", got)
	}
}

func TestClassifyDegrades(t *testing.T) {
	// NaN / Inf 降级为文本，原因 bad_number。
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		cv, reason := classify(f, CellEmpty)
		if cv.typ != CellText || reason != ReasonBadNumber {
			t.Fatalf("float %v: typ=%v reason=%v", f, cv.typ, reason)
		}
	}
	// 不支持的类型降级为文本，原因 unsupported_kind。
	cv, reason := classify(struct{ X int }{1}, CellEmpty)
	if cv.typ != CellText || reason != ReasonUnsupportedKind {
		t.Fatalf("struct: typ=%v reason=%v", cv.typ, reason)
	}
	if formatText(cv) != "{1}" {
		t.Fatalf("struct text = %q", formatText(cv))
	}
}

func TestForcedColumnType(t *testing.T) {
	// 强制数值列：数字字符串可转换。
	cv, reason := classify("42.5", CellNumber)
	if reason != "" || cv.typ != CellNumber || formatText(cv) != "42.5" {
		t.Fatalf("coerce number: typ=%v reason=%v text=%q", cv.typ, reason, formatText(cv))
	}
	// 强制数值列：无法转换的字符串降级为文本。
	cv, reason = classify("abc", CellNumber)
	if cv.typ != CellText || reason != ReasonTypeCoerceFail || formatText(cv) != "abc" {
		t.Fatalf("coerce fail: typ=%v reason=%v text=%q", cv.typ, reason, formatText(cv))
	}
	// 强制文本列：以 = 开头的字符串不再被当作公式。
	cv, reason = classify("=1+1", CellText)
	if reason != "" || cv.typ != CellText || formatText(cv) != "=1+1" {
		t.Fatalf("forced text: typ=%v reason=%v", cv.typ, reason)
	}
	// 强制布尔列。
	cv, _ = classify("true", CellBool)
	if cv.typ != CellBool || formatText(cv) != "true" {
		t.Fatalf("coerce bool: typ=%v", cv.typ)
	}
	// 强制日期列。
	cv, reason = classify("2024-01-02T03:04:05Z", CellDate)
	if reason != "" || cv.typ != CellDate || formatText(cv) != "2024-01-02T03:04:05Z" {
		t.Fatalf("coerce date: typ=%v reason=%v", cv.typ, reason)
	}
	// 强制公式列：缺少 = 前缀时补上。
	cv, _ = classify("SUM(A1:A2)", CellFormula)
	if cv.typ != CellFormula || formatText(cv) != "=SUM(A1:A2)" {
		t.Fatalf("coerce formula: typ=%v text=%q", cv.typ, formatText(cv))
	}
}

func TestClassifyDeterministicAcrossValues(t *testing.T) {
	// 同样的值重复分类必须得到同样的结果（与调用次数无关）。
	for i := 0; i < 100; i++ {
		cv1, r1 := classify("dup", CellEmpty)
		cv2, r2 := classify("dup", CellEmpty)
		if cv1 != cv2 || r1 != r2 {
			t.Fatal("classify not deterministic")
		}
	}
}

func TestColRef(t *testing.T) {
	cases := map[int]string{0: "A", 25: "Z", 26: "AA", 27: "AB", 701: "ZZ", 702: "AAA"}
	for in, want := range cases {
		if got := colRef(in); got != want {
			t.Fatalf("colRef(%d) = %q, want %q", in, got, want)
		}
	}
}
