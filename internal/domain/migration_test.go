package domain

import (
	"encoding/json"
	"testing"
)

func TestBatchConfigJSON(t *testing.T) {
	in := BatchConfig{Table: "public.orders", CursorColumn: "id", BatchSize: 1000}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"table":"public.orders","cursor_column":"id","batch_size":1000}`
	if string(data) != want {
		t.Fatalf("json = %s, want %s", data, want)
	}

	var out BatchConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}
