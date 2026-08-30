package q3

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverCatalogReadsMapsAndArenaGameTypes(t *testing.T) {
	base := t.TempDir()
	archivePath := filepath.Join(base, "custom.pk3")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(archive)
	files := map[string]string{
		"maps/ospctf1.bsp": "fixture",
		"maps/ospdm1.bsp":  "fixture",
		"maps/unknown.bsp": "fixture",
		"scripts/custom.arena": `{
map "ospctf1"
type "ctf team"
}
{
map ospdm1
type ffa team
}`,
	}
	for name, body := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	catalog, err := DiscoverCatalog(base)
	if err != nil {
		t.Fatal(err)
	}
	want := []MapInfo{
		{Name: "ospctf1", GameTypes: []int{GameTypeTDM, GameTypeCTF}},
		{Name: "ospdm1", GameTypes: []int{GameTypeFFA, GameTypeTDM}},
		{Name: "unknown", GameTypes: nil},
	}
	if !reflect.DeepEqual(catalog, want) {
		t.Fatalf("DiscoverCatalog() = %#v, want %#v", catalog, want)
	}
}

func TestLegacyModeInferenceIsNarrow(t *testing.T) {
	if got := inferredGameTypes("actf01"); !reflect.DeepEqual(got, []int{GameTypeCTF}) {
		t.Fatalf("actf01 modes = %#v", got)
	}
	if got := inferredGameTypes("batcula"); !reflect.DeepEqual(got, []int{GameTypeTDM}) {
		t.Fatalf("batcula modes = %#v", got)
	}
	if got := inferredGameTypes("unknown"); got != nil {
		t.Fatalf("unknown modes = %#v, want nil", got)
	}
}

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
