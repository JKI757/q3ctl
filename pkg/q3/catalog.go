package q3

import (
	"archive/zip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var mapName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// DiscoverMaps lists loadable BSP maps from installed pk3 archives and
// development-style pk3dir directories. It never trusts names from a client.
func DiscoverMaps(basePath string) ([]string, error) {
	if basePath == "" {
		return nil, errors.New("game data path is required")
	}
	found := make(map[string]struct{})
	add := func(name string) {
		name = strings.ToLower(name)
		if mapName.MatchString(name) {
			found[name] = struct{}{}
		}
	}
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		path := filepath.Join(basePath, entry.Name())
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".pk3") && !entry.IsDir() {
			archive, err := zip.OpenReader(path)
			if err != nil {
				continue
			}
			for _, file := range archive.File {
				addMapPath(file.Name, add)
			}
			archive.Close()
			continue
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".pk3dir") && entry.IsDir() {
			_ = filepath.WalkDir(path, func(filename string, entry fs.DirEntry, err error) error {
				if err == nil && !entry.IsDir() {
					addMapPath(strings.TrimPrefix(filename, path+string(filepath.Separator)), add)
				}
				return nil
			})
		}
	}
	maps := make([]string, 0, len(found))
	for name := range found {
		maps = append(maps, name)
	}
	sort.Strings(maps)
	return maps, nil
}

func addMapPath(path string, add func(string)) {
	path = strings.ReplaceAll(path, "\\", "/")
	if !strings.HasPrefix(strings.ToLower(path), "maps/") || !strings.HasSuffix(strings.ToLower(path), ".bsp") {
		return
	}
	add(strings.TrimSuffix(strings.TrimPrefix(path, "maps/"), ".bsp"))
}

func ContainsMap(maps []string, name string) bool {
	for _, candidate := range maps {
		if candidate == name {
			return true
		}
	}
	return false
}
