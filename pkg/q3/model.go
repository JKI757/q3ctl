// Package q3 contains validated domain types shared by q3ctl components.
package q3

import (
	"errors"
	"strings"
)

const (
	GameTypeFFA        = 0
	GameTypeTournament = 1
	GameTypeTDM        = 3
	GameTypeCTF        = 4
)

var StockMaps = []string{"q3ctf1", "q3ctf2", "q3ctf3", "q3ctf4", "q3dm1", "q3dm6", "q3dm7", "q3dm17", "q3dm18", "q3dm19"}

type RotationEntry struct {
	Map          string `json:"map"`
	GameType     int    `json:"gametype"`
	TimeLimit    int    `json:"timelimit"`
	FragLimit    int    `json:"fraglimit"`
	CaptureLimit int    `json:"capturelimit"`
}

type Rotation []RotationEntry

func KnownMap(name string) bool {
	for _, candidate := range StockMaps {
		if name == candidate {
			return true
		}
	}
	return false
}

func ValidGameType(gameType int) bool {
	return gameType == GameTypeFFA || gameType == GameTypeTournament || gameType == GameTypeTDM || gameType == GameTypeCTF
}

func (r Rotation) Validate() error {
	if len(r) == 0 || len(r) > 12 {
		return errors.New("rotation must contain 1 to 12 entries")
	}
	for _, entry := range r {
		if !KnownMap(entry.Map) || !ValidGameType(entry.GameType) {
			return errors.New("rotation contains an invalid map or mode")
		}
		if entry.TimeLimit < 0 || entry.TimeLimit > 120 || entry.FragLimit < 0 || entry.FragLimit > 999 || entry.CaptureLimit < 0 || entry.CaptureLimit > 99 {
			return errors.New("rotation contains an invalid match limit")
		}
	}
	return nil
}

// NextMapCommands returns only safe, fully validated Quake console commands.
// It defines a closed d1..dN chain and changes the active server's next map;
// it deliberately does not load a map or interrupt the current match.
func (r Rotation) NextMapCommands() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	commands := make([]string, 0, len(r)+1)
	for i, entry := range r {
		next := i + 2
		if next > len(r) {
			next = 1
		}
		commands = append(commands, "set d"+itoa(i+1)+" \"set g_gametype "+itoa(entry.GameType)+"; set fraglimit "+itoa(entry.FragLimit)+"; set capturelimit "+itoa(entry.CaptureLimit)+"; set timelimit "+itoa(entry.TimeLimit)+"; map "+entry.Map+"; set nextmap vstr d"+itoa(next)+"\"")
	}
	commands = append(commands, "set nextmap vstr d1")
	return strings.Join(commands, "; "), nil
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var out [12]byte
	i := len(out)
	for n > 0 {
		i--
		out[i] = digits[n%10]
		n /= 10
	}
	return string(out[i:])
}
