package main

import (
	"os"
	"testing"
)

func TestParseStatus(t *testing.T) {
	raw := `\mapname\q3ctf1\g_gametype\4\timelimit\20\fraglimit\0\capturelimit\8\sv_maxclients\16
num score ping name            address               rate
--- ----- ---- --------------- --------------------- -----
  0    12    0 Sarge           ^7bot                 16384
  1     8   52 Joshua          198.51.100.2:27960    25000
`
	s := parseStatus(raw)
	if s.Map != "q3ctf1" || s.GameType != 4 || s.MaxClients != 16 {
		t.Fatalf("unexpected status: %#v", s)
	}
	if len(s.Players) != 2 || !s.Players[0].Bot || s.Players[1].Bot || s.Players[1].Name != "Joshua" {
		t.Fatalf("unexpected players: %#v", s.Players)
	}
}

func TestStripQ3Colors(t *testing.T) {
	if got := stripQ3Colors("^1A^2n^3a^4r^5k^6i"); got != "Anarki" {
		t.Fatalf("got %q, want Anarki", got)
	}
	if got := stripQ3Colors("Sarge"); got != "Sarge" {
		t.Fatalf("got %q", got)
	}
}

func TestReadGameLog(t *testing.T) {
	path := t.TempDir() + "/q3games.log"
	if err := os.WriteFile(path, []byte("InitGame: \\mapname\\q3dm6\nClientConnect: 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var offset int64
	lines, err := readGameLog(path, &offset, true)
	if err != nil || len(lines) != 2 || lines[1] != "ClientConnect: 1" {
		t.Fatalf("initial tail: lines=%q offset=%d err=%v", lines, offset, err)
	}
	if err := os.WriteFile(path, []byte("InitGame: \\mapname\\q3dm6\nClientConnect: 1\nKill: 1 2 7: Anarki killed Sarge\n"), 0644); err != nil {
		t.Fatal(err)
	}
	lines, err = readGameLog(path, &offset, false)
	if err != nil || len(lines) != 1 || lines[0] != "Kill: 1 2 7: Anarki killed Sarge" {
		t.Fatalf("incremental tail: lines=%q offset=%d err=%v", lines, offset, err)
	}
}

func TestValidatePolicy(t *testing.T) {
	p := defaults().Policy
	if err := validatePolicy(p); err != nil {
		t.Fatal(err)
	}
	p.HumanTeam = "green"
	if err := validatePolicy(p); err == nil {
		t.Fatal("expected invalid team")
	}
}

func TestSafeToken(t *testing.T) {
	if got := safeToken("Sarge; quit"); got != "Sargequit" {
		t.Fatalf("got %q", got)
	}
}
