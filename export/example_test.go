package export_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	export "github.com/shikejing/report-export-engine-go/export"
)

func Example() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)

	eng, err := export.NewEngine(export.Config{TempDir: dir})
	if err != nil {
		panic(err)
	}
	x, err := eng.Export(context.Background(), []export.SheetDef{
		{Name: "orders", Columns: []export.Column{
			{Name: "id"}, {Name: "amount"}, {Name: "paid"}, {Name: "created_at"},
		}},
	})
	if err != nil {
		panic(err)
	}
	w, _ := x.SheetWriter("orders")
	_ = w.WriteRow(context.Background(), []any{1, 9.5, true, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)})
	_ = w.WriteRow(context.Background(), []any{2, 3.25, false, time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)})
	_ = w.Close()

	res, err := x.Commit(context.Background(), filepath.Join(dir, "report.xlsx"), filepath.Join(dir, "csv"))
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Sheets[0].Name, res.Sheets[0].Rows, filepath.Base(res.Sheets[0].CSVPath))
	// Output: orders 2 01_orders.csv
}
