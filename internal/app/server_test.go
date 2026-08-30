package app

import (
	"net"
	"os"
	"strings"
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

func TestParseInfoString(t *testing.T) {
	info := parseInfoString(`statusResponse
\mapname\q3ctf1\g_gametype\4\sv_hostname	est`)
	if info["mapname"] != "q3ctf1" || info["g_gametype"] != "4" {
		t.Fatalf("unexpected info string: %#v", info)
	}
	if got := parseInfoString("statusResponse\nno info here"); got != nil {
		t.Fatalf("got %#v, want nil", got)
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
	p = defaults().Policy
	p.BotsPerTeam = 5
	if err := validatePolicy(p); err == nil {
		t.Fatal("expected target beyond roster to be rejected")
	}
}

func TestTeamNameFromUserInfo(t *testing.T) {
	sep := string('\\')
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"userinfo\n--------\n" + strings.Join([]string{"", "name", "Sarge", "t", "1", "skill", "3"}, sep), "red"},
		{strings.Join([]string{"", "name", "Hunter", "t", "2"}, sep), "blue"},
		{strings.Join([]string{"", "name", "Spec", "t", "3"}, sep), "spectator"},
		{"no info", ""},
	} {
		if got := teamNameFromUserInfo(tc.raw); got != tc.want {
			t.Errorf("teamNameFromUserInfo(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestBotCounts(t *testing.T) {
	counts := botCounts([]Player{
		{Name: "Sarge", Bot: true, Team: "red"},
		{Name: "Hunter", Bot: true, Team: "blue"},
		{Name: "Major", Bot: true, Team: "red"},
		{Name: "Joshua", Bot: false, Team: "red"},
	}, 2)
	if counts.TargetPerTeam != 2 || counts.Total != 3 || counts.Red != 2 || counts.Blue != 1 {
		t.Fatalf("unexpected bot counts: %#v", counts)
	}
}

func TestIsTimeout(t *testing.T) {
	if !isTimeout(&net.DNSError{IsTimeout: true}) {
		t.Fatal("expected timeout to be recognized")
	}
	if isTimeout(&net.DNSError{}) {
		t.Fatal("unexpected timeout recognition")
	}
}

func TestSafeToken(t *testing.T) {
	if got := safeToken("Sarge; quit"); got != "Sargequit" {
		t.Fatalf("got %q", got)
	}
}
