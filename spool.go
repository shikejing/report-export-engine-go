package reportexport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// encodedCell is the on-disk JSON shape of a canonical Cell. Field order is
// fixed by the struct, so a given row always serializes to identical bytes
// regardless of map iteration or duplicate-value bookkeeping.
type encodedCell struct {
	K int          `json:"k"`
	T string       `json:"t,omitempty"`
	N string       `json:"n,omitempty"`
	B bool         `json:"b,omitempty"`
	F string       `json:"f,omitempty"`
	C *encodedCell `json:"c,omitempty"`
	S float64      `json:"s,omitempty"`
	D bool         `json:"d,omitempty"`
}

func encodeCell(c Cell) encodedCell {
	e := encodedCell{K: int(c.Kind), T: c.Text, N: c.Number, B: c.Bool, F: c.Formula, S: c.Date.Serial, D: c.Date.Date}
	if c.Cached != nil {
		cc := encodeCell(*c.Cached)
		e.C = &cc
	}
	return e
}

func decodeCell(e encodedCell) Cell {
	c := Cell{
		Kind:    Kind(e.K),
		Text:    e.T,
		Number:  e.N,
		Bool:    e.B,
		Formula: e.F,
	}
	c.Date.Serial = e.S
	c.Date.Date = e.D
	if e.C != nil {
		cc := decodeCell(*e.C)
		c.Cached = &cc
	}
	return c
}

// spool is a length-delimited JSON-lines on-disk buffer of canonical Cells.
//
// Rows are encoded as soon as the in-memory batch is full; reads stream the
// file back one line at a time. Memory use is therefore bounded by BatchSize
// (and a single encoded line), independent of total row count. Files live in
// Config.TempDir and are removed by Close on every path — success, failure or
// cancellation.
type spool struct {
	path    string
	f       *os.File
	w       *bufio.Writer
	batch   int
	pending int
	closed  bool
}

func newSpool(dir string, batch int) (*spool, error) {
	f, err := os.CreateTemp(dir, "reportexport-spool-*.jsonl")
	if err != nil {
		return nil, err
	}
	return &spool{
		path:  f.Name(),
		f:     f,
		w:     bufio.NewWriterSize(f, 64*1024),
		batch: batch,
	}, nil
}

// WriteRow appends one row. It honors ctx so a canceled export stops
// immediately instead of draining more data.
func (s *spool) WriteRow(ctx context.Context, cells []Cell) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return ErrClosed
	}
	enc := make([]encodedCell, len(cells))
	for i, c := range cells {
		enc[i] = encodeCell(c)
	}
	data, err := json.Marshal(enc)
	if err != nil {
		return err
	}
	// Length prefix keeps framing independent of content (a cell may contain
	// newlines) and lets the reader bound line memory via ReadFull.
	if _, err := s.w.WriteString(itoa(len(data))); err != nil {
		return err
	}
	if err := s.w.WriteByte(':'); err != nil {
		return err
	}
	if _, err := s.w.Write(data); err != nil {
		return err
	}
	if err := s.w.WriteByte('\n'); err != nil {
		return err
	}
	s.pending++
	if s.pending >= s.batch {
		s.pending = 0
		return s.w.Flush()
	}
	return nil
}

// Rows streams every spooled row back in write order.
func (s *spool) Rows(ctx context.Context, fn func([]Cell) error) error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(s.f, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) && line == "" {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		line = trimEOL(line)
		colon := indexColon(line)
		if colon < 0 {
			return io.ErrUnexpectedEOF
		}
		n, perr := atoi(line[:colon])
		if perr != nil || n < 0 || colon+1+n > len(line) {
			return io.ErrUnexpectedEOF
		}
		payload := line[colon+1 : colon+1+n]
		var enc []encodedCell
		if jerr := json.Unmarshal([]byte(payload), &enc); jerr != nil {
			return jerr
		}
		cells := make([]Cell, len(enc))
		for i, e := range enc {
			cells[i] = decodeCell(e)
		}
		if ferr := fn(cells); ferr != nil {
			return ferr
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

// Close flushes, closes and unlinks the spool. It is idempotent and always
// attempts removal, even if the close itself fails.
func (s *spool) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var firstErr error
	if err := s.w.Flush(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := s.f.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func indexColon(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, io.ErrUnexpectedEOF
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, io.ErrUnexpectedEOF
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// spoolDir creates a fresh per-export temp directory under base. A unique
// directory means concurrent exports and retries never share partial files.
func spoolDir(base string) (string, error) {
	return os.MkdirTemp(base, "reportexport-*")
}
