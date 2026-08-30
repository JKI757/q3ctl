package q3

import (
	"strings"
	"testing"
)

func TestRotationNextMapCommands(t *testing.T) {
	rotation := Rotation{
		{Map: "q3ctf1", GameType: GameTypeCTF, TimeLimit: 20, FragLimit: 0, CaptureLimit: 8},
		{Map: "q3dm17", GameType: GameTypeTDM, TimeLimit: 15, FragLimit: 40, CaptureLimit: 0},
	}
	commands, err := rotation.NextMapCommands()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`set d1 "set g_gametype 4; set fraglimit 0; set capturelimit 8; set timelimit 20; map q3ctf1; set nextmap vstr d2"`,
		`set d2 "set g_gametype 3; set fraglimit 40; set capturelimit 0; set timelimit 15; map q3dm17; set nextmap vstr d1"`,
		`set nextmap vstr d1`,
	} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("missing %q in %q", expected, commands)
		}
	}
	if strings.Contains(commands, "map q3ctf1; map") {
		t.Fatal("unexpected immediate map load")
	}
}

func TestRotationCommandList(t *testing.T) {
	rotation := Rotation{
		{Map: "q3ctf1", GameType: GameTypeCTF, TimeLimit: 20, CaptureLimit: 8},
		{Map: "q3dm17", GameType: GameTypeTDM, TimeLimit: 15, FragLimit: 40},
	}
	commands, err := rotation.NextMapCommandListWithMaps([]string{"q3ctf1", "q3dm17"})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 3 || !strings.Contains(commands[0], "set d1") || !strings.Contains(commands[1], "set d2") || commands[2] != "set nextmap vstr d1" {
		t.Fatalf("unexpected command list: %#v", commands)
	}
}

func TestRotationRejectsUnsafeEntries(t *testing.T) {
	if err := (Rotation{{Map: `q3ctf1; quit`, GameType: GameTypeCTF}}).Validate(); err == nil {
		t.Fatal("unsafe map accepted")
	}
	if err := (Rotation{{Map: "q3ctf1", GameType: 99}}).Validate(); err == nil {
		t.Fatal("invalid mode accepted")
	}
}
