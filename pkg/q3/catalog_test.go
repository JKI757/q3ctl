package q3

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverMapsFromPackagesAndDirectories(t *testing.T) {
	base := t.TempDir()
	archivePath := filepath.Join(base, "custom.pk3")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(archive)
	for _, name := range []string{"maps/ospctf1.bsp", "maps/ospdm1.bsp", "scripts/ospctf1.shader", "maps/NOT A MAP.bsp"} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("fixture")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "dev.pk3dir", "maps"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "dev.pk3dir", "maps", "local_test.bsp"), []byte("fixture"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := DiscoverMaps(base)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"local_test", "ospctf1", "ospdm1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverMaps() = %#v, want %#v", got, want)
	}
}

func TestDiscoverMapsRejectsMissingPath(t *testing.T) {
	if _, err := DiscoverMaps(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected missing directory error")
	}
}
