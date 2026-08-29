package main

import "testing"

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
