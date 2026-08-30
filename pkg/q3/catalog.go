package q3

import (
	"archive/zip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	mapName     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	arenaBlocks = regexp.MustCompile(`(?s)\{(.*?)\}`)
	arenaField  = regexp.MustCompile(`(?mi)^\s*(map|type)\s+"?([^"\r\n]+)"?\s*$`)
)

// MapInfo is a loadable map and the standard Arena game types declared by its
// installed arena metadata. GameTypes is empty when an old/custom map supplies
// no declaration; callers should not guess its intended mode.
type MapInfo struct {
	Name      string `json:"name"`
	GameTypes []int  `json:"gametypes"`
}

// DiscoverCatalog lists loadable BSP maps and their installed arena-declared
// modes from pk3 archives and development-style pk3dir directories. It never
// trusts map names from a client and skips unreadable/corrupt individual files.
func DiscoverCatalog(basePath string) ([]MapInfo, error) {
	if basePath == "" {
		return nil, errors.New("game data path is required")
	}
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return nil, err
	}
	found := map[string]map[int]struct{}{}
	addMap := func(name string) {
		name = strings.ToLower(name)
		if mapName.MatchString(name) {
			if _, ok := found[name]; !ok {
				found[name] = map[int]struct{}{}
			}
		}
	}
	addArena := func(data string) {
		for _, block := range arenaBlocks.FindAllStringSubmatch(data, -1) {
			fields := map[string]string{}
			for _, match := range arenaField.FindAllStringSubmatch(block[1], -1) {
				fields[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
			}
			name := strings.ToLower(fields["map"])
			if !mapName.MatchString(name) {
				continue
			}
			addMap(name)
			for _, gameType := range gameTypesFromArena(fields["type"]) {
				found[name][gameType] = struct{}{}
			}
		}
	}
	for _, entry := range entries {
		path := filepath.Join(basePath, entry.Name())
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".pk3") && !entry.IsDir() {
			archive, err := zip.OpenReader(path)
			if err != nil {
				continue
			}
			for _, file := range archive.File {
				addMapPath(file.Name, addMap)
				if isArenaPath(file.Name) {
					if reader, openErr := file.Open(); openErr == nil {
						data, readErr := io.ReadAll(io.LimitReader(reader, 1<<20))
						reader.Close()
						if readErr == nil {
							addArena(string(data))
						}
					}
				}
			}
			archive.Close()
			continue
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".pk3dir") && entry.IsDir() {
			_ = filepath.WalkDir(path, func(filename string, child fs.DirEntry, walkErr error) error {
				if walkErr != nil || child.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(path, filename)
				addMapPath(rel, addMap)
				if isArenaPath(rel) {
					if data, readErr := os.ReadFile(filename); readErr == nil {
						addArena(string(data))
					}
				}
				return nil
			})
		}
	}
	catalog := make([]MapInfo, 0, len(found))
	for name, types := range found {
		// JSON should consistently expose an array. A nil slice becomes `null`,
		// which forces every browser client to special-case unclassified maps.
		info := MapInfo{Name: name, GameTypes: make([]int, 0, len(types))}
		for gameType := range types {
			info.GameTypes = append(info.GameTypes, gameType)
		}
		if len(info.GameTypes) == 0 {
			if inferred := inferredGameTypes(name); inferred != nil {
				info.GameTypes = inferred
			}
		}
		sort.Ints(info.GameTypes)
		catalog = append(catalog, info)
	}
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].Name < catalog[j].Name })
	return catalog, nil
}

func inferredGameTypes(name string) []int {
	// Some respected legacy packs predate Quake's .arena declarations. These
	// overrides are deliberately narrow: unknown maps remain unclassified
	// rather than being shown as compatible with a mode they may not support.
	ctfOnly := map[string]bool{
		"bastir": true, "frozencolors": true, "mapel4b": true, "kellblack": true,
		"mikectf3": true, "mikectf3temp": true, "mkbase": true,
	}
	tdmOnly := map[string]bool{"batcula": true, "ci": true, "mkexp": true, "teddm2": true, "ts_dm5tmp": true}
	if ctfOnly[name] || strings.HasPrefix(name, "actf") || strings.Contains(name, "ctf") {
		return []int{GameTypeCTF}
	}
	if tdmOnly[name] || strings.Contains(name, "dm") {
		return []int{GameTypeTDM}
	}
	return nil
}

func gameTypesFromArena(value string) []int {
	seen := map[int]struct{}{}
	for _, token := range strings.Fields(strings.ToLower(value)) {
		switch token {
		case "ffa", "single":
			seen[GameTypeFFA] = struct{}{}
		case "tourney", "tournament":
			seen[GameTypeTournament] = struct{}{}
		case "team", "tdm":
			seen[GameTypeTDM] = struct{}{}
		case "ctf":
			seen[GameTypeCTF] = struct{}{}
		}
	}
	out := make([]int, 0, len(seen))
	for gameType := range seen {
		out = append(out, gameType)
	}
	sort.Ints(out)
	return out
}

func isArenaPath(path string) bool {
	path = strings.ReplaceAll(strings.ToLower(path), "\\", "/")
	return strings.HasPrefix(path, "scripts/") && strings.HasSuffix(path, ".arena")
}

func addMapPath(path string, add func(string)) {
	path = strings.ReplaceAll(path, "\\", "/")
	if !strings.HasPrefix(strings.ToLower(path), "maps/") || !strings.HasSuffix(strings.ToLower(path), ".bsp") {
		return
	}
	add(strings.TrimSuffix(strings.TrimPrefix(path, "maps/"), ".bsp"))
}

// DiscoverMaps preserves the compact name-only API used by validation paths.
func DiscoverMaps(basePath string) ([]string, error) {
	catalog, err := DiscoverCatalog(basePath)
	if err != nil {
		return nil, err
	}
	maps := make([]string, len(catalog))
	for i, info := range catalog {
		maps[i] = info.Name
	}
	return maps, nil
}

func ContainsMap(maps []string, name string) bool {
	for _, candidate := range maps {
		if candidate == name {
			return true
		}
	}
	return false
}
