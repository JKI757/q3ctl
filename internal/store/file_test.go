package store

import (
	"os"
	"path/filepath"
	"testing"
)

type fixture struct {
	Name string `json:"name"`
}

func TestFileSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	file := File[fixture]{Path: path}
	if err := file.Save(fixture{Name: "persisted"}); err != nil {
		t.Fatal(err)
	}
	value, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if value.Name != "persisted" {
		t.Fatalf("got %#v", value)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("state mode = %o, want 640", info.Mode().Perm())
	}
}
