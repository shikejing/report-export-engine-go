package reportexport

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
)

// CSV field semantics (shared by XLSX text handling at the value layer):
//
//   - Fields are separated by ',' and records are terminated by CRLF, per
//     RFC 4180.
//   - A field is quoted when it contains '"', CR, LF or ','.
//   - Inside quotes, '"' is doubled ("").
//
// Field text is always taken from Cell.displayText() — the same string used
// for XLSX inline content — so values and escaping-visible characters cannot
// diverge between the two formats.

func needsQuote(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', ',', '\n', '\r':
			return true
		}
	}
	return false
}

func writeCSVField(w io.Writer, s string) error {
	if !needsQuote(s) {
		_, err := io.WriteString(w, s)
		return err
	}
	if _, err := io.WriteString(w, "\""); err != nil {
		return err
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			if _, err := io.WriteString(w, "\"\""); err != nil {
				return err
			}
		} else {
			if err := w.(io.ByteWriter).WriteByte(s[i]); err != nil {
				return err
			}
		}
	}
	_, err := io.WriteString(w, "\"")
	return err
}

func writeCSVHeader(w *bufio.Writer, cols []Column) error {
	for i, c := range cols {
		if i > 0 {
			if err := w.WriteByte(','); err != nil {
				return err
			}
		}
		if err := writeCSVField(w, sanitizeForOutput(c.Name)); err != nil {
			return err
		}
	}
	_, err := w.WriteString("\r\n")
	return err
}

func writeCSVRow(w *bufio.Writer, cells []Cell) error {
	for i, c := range cells {
		if i > 0 {
			if err := w.WriteByte(','); err != nil {
				return err
			}
		}
		if err := writeCSVField(w, c.displayText()); err != nil {
			return err
		}
	}
	_, err := w.WriteString("\r\n")
	return err
}

// writeCSV streams one sheet's spool into path atomically. Data is written to
// path+".partial" in dir, synced, then renamed over path. On cancel or error
// the partial file is removed and nothing is left at path.
//
// An empty sheet still produces a file containing just the header line; an
// empty row in the input produces a blank record, preserving row positions.
func writeCSV(ctx context.Context, dir, path string, cols []Column, sp *spool, batch int, failed bool) (err error) {
	tmp := path + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	if err := writeCSVHeader(bw, cols); err != nil {
		cleanup()
		return err
	}
	count := 0
	if !failed {
		err = sp.Rows(ctx, func(cells []Cell) error {
			if err := writeCSVRow(bw, cells); err != nil {
				return err
			}
			count++
			if count%batch == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				return bw.Flush()
			}
			return nil
		})
	}
	if err != nil {
		cleanup()
		return err
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
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
	// fsync the directory so the rename itself is durable.
	if d, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
