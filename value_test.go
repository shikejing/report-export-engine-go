package reportexport

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestInferCellRules(t *testing.T) {
	now := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	cases := []struct {
		name    string
		in      any
		kind    Kind
		display string
		reason  Reason
	}{
		{"nil", nil, KindBlank, "", ""},
		{"string stays text", "=not-a-formula", KindText, "=not-a-formula", ""},
		{"string number stays text", "123", KindText, "123", ""},
		{"bool", true, KindBool, "TRUE", ""},
		{"int", int64(-9), KindNumber, "-9", ""},
		{"uint", uint(42), KindNumber, "42", ""},
		{"float", 3.5, KindNumber, "3.5", ""},
		{"float integral canonicalized", 1.0, KindNumber, "1", ""},
		{"json number", json.Number("1.25"), KindNumber, "1.25", ""},
		{"bytes", []byte("hi"), KindText, "hi", ""},
		{"datetime", now, KindDate, "2024-03-04 05:06:07", ""},
		{"dateonly", AsDateOnly(now), KindDate, "2024-03-04", ""},
		{"nan", math.NaN(), KindText, "NaN", ReasonNumberOutOfRange},
		{"inf", math.Inf(1), KindText, "+Inf", ReasonNumberOutOfRange},
		{"struct unsupported", struct{ X int }{1}, KindText, "{1}", ReasonUnsupportedType},
		{"map unsupported", map[string]int{"a": 1}, KindText, "map[a:1]", ReasonUnsupportedType},
		{"nil pointer", (*int)(nil), KindBlank, "", ""},
		{"asnumber ok", AsNumber(7), KindNumber, "7", ""},
		{"asnumber bad", AsNumber("x"), KindText, "x", ReasonNumberInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, r, _ := inferCell(tc.in)
			if c.Kind != tc.kind {
				t.Fatalf("kind=%d want %d", c.Kind, tc.kind)
			}
			if got := c.displayText(); got != tc.display {
				t.Fatalf("display=%q want %q", got, tc.display)
			}
			if r != tc.reason {
				t.Fatalf("reason=%q want %q", r, tc.reason)
			}
		})
	}
}

func TestFormulaCachedDegradation(t *testing.T) {
	// unsupported cached value degrades to text inside the formula but still
	// returns a distinguishable reason
	c, r, _ := inferCell(AsFormula("A1", struct{}{}))
	if c.Kind != KindFormula {
		t.Fatalf("kind=%v", c.Kind)
	}
	if c.Cached == nil || c.Cached.Kind != KindText {
		t.Fatalf("cached=%+v", c.Cached)
	}
	if r != ReasonFormulaCachedInvalid {
		t.Fatalf("reason=%v", r)
	}
	if got := c.displayText(); got != "{}" {
		t.Fatalf("display=%q", got)
	}
}

func TestDuplicateValuesCanonical(t *testing.T) {
	// Repeated values and integer-valued floats must share one canonical
	// spelling, never depend on encounter order or cache capacity.
	vals := []any{float64(1), 1, int64(1), uint(1), json.Number("1"), AsNumber(1.0)}
	first := ""
	for i, v := range vals {
		c, reason, _ := inferCell(v)
		if reason != "" {
			t.Fatalf("unexpected reason %v for %v", reason, v)
		}
		if c.Number != "1" {
			t.Fatalf("value %d canonicalized to %q", i, c.Number)
		}
		if first == "" {
			first = c.Number
		} else if c.Number != first {
			t.Fatalf("inconsistent canonical spelling: %q vs %q", c.Number, first)
		}
	}
	// reverse order yields identical results
	for i := len(vals) - 1; i >= 0; i-- {
		c, _, _ := inferCell(vals[i])
		if c.Number != "1" {
			t.Fatalf("reverse order changed result: %q", c.Number)
		}
	}
}

func TestDateSerialRoundTrip(t *testing.T) {
	// serial for an instant must round-trip back to the same UTC wall time.
	in := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	s := excelSerial(in)
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	back := base.Add(time.Duration(s * float64(24*time.Hour)))
	if !back.Equal(in) {
		t.Fatalf("serial %v round trip: %v want %v", s, back, in)
	}
	// date-only serial is an exact integer
	c, _, _ := inferCell(AsDateOnly(in))
	if c.Date.Serial != float64(int64(c.Date.Serial)) {
		t.Fatalf("date-only serial not integral: %v", c.Date.Serial)
	}
}

func TestSanitizeSharedByFormats(t *testing.T) {
	c, _, _ := inferCell("a\x00b\x01c")
	got := c.displayText()
	if got != "a�b�c" {
		t.Fatalf("sanitized=%q", got)
	}
	// tab/newline/cr preserved (legal content; entities are XML-only concern)
	c, _, _ = inferCell("t\tx\ny\rz")
	if c.displayText() != "t\tx\ny\rz" {
		t.Fatalf("legal controls altered: %q", c.displayText())
	}
}

type customValue struct{}

func (customValue) AsCell() (Cell, error) { return Cell{Kind: KindNumber, Number: "42"}, nil }

type customBad struct{}

func (customBad) AsCell() (Cell, error) { return Cell{}, errBoom }

type errString string

func (e errString) Error() string { return string(e) }

const errBoom errString = "boom"

func TestCellValueInterface(t *testing.T) {
	c, r, _ := inferCell(customValue{})
	if c.Kind != KindNumber || c.Number != "42" || r != "" {
		t.Fatalf("custom: %+v %q", c, r)
	}
	c, r, detail := inferCell(customBad{})
	if c.Kind != KindText || r != ReasonUnsupportedType || detail != "boom" {
		t.Fatalf("custom bad: %+v %q %q", c, r, detail)
	}
}

func TestRowPaddingAndOverflow(t *testing.T) {
	// Engine-level: short rows pad with blanks; long rows drop extras and
	// warn with a stable reason and the exact row number.
	// (covered via Engine in engine_integration_test.go)
}
