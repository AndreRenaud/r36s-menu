package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func rom(size int, put map[int][]byte) []byte {
	b := make([]byte, size)
	for off, data := range put {
		copy(b[off:], data)
	}
	return b
}

func snesHeader() []byte {
	h := make([]byte, 0x20)
	copy(h, "SUPER TEST GAME      ")
	h[0x1C], h[0x1D] = 0x34, 0x12 // complement
	h[0x1E], h[0x1F] = 0xCB, 0xED // checksum
	return h
}

var sniffCases = []struct {
	name string
	data []byte
	want System
}{
	{"nes", rom(0x4010, map[int][]byte{0: []byte("NES\x1a")}), SysNES},
	{"gb", rom(0x8000, map[int][]byte{0x104: nintendoLogoStart}), SysGB},
	{"gbc", rom(0x8000, map[int][]byte{0x104: nintendoLogoStart, 0x143: {0xC0}}), SysGBC},
	{"gba", rom(0x10000, map[int][]byte{4: gbaLogoStart, 0xB2: {0x96}}), SysGBA},
	{"megadrive", rom(0x10000, map[int][]byte{0x100: []byte("SEGA MEGA DRIVE ")}), SysMegaDrive},
	{"sms", rom(0x8000, map[int][]byte{0x7FF0: []byte("TMR SEGA"), 0x7FFF: {0x4C}}), SysMasterSystem},
	{"gamegear", rom(0x8000, map[int][]byte{0x7FF0: []byte("TMR SEGA"), 0x7FFF: {0x6C}}), SysGameGear},
	{"snes lorom", rom(0x10000, map[int][]byte{0x7FC0: snesHeader()}), SysSNES},
	{"snes hirom", rom(0x10000, map[int][]byte{0xFFC0: snesHeader()}), SysSNES},
	{"snes with copier header", rom(0x10200, map[int][]byte{0x81C0: snesHeader()}), SysSNES},
	{"n64", rom(0x10000, map[int][]byte{0: {0x80, 0x37, 0x12, 0x40}}), SysN64},
	{"n64 byte-swapped", rom(0x10000, map[int][]byte{0: {0x37, 0x80, 0x40, 0x12}}), SysN64},
	{"ps1 raw disc", rom(0x10000, map[int][]byte{0: cdSync, 16*2352 + 25: []byte("CD001"), 16*2352 + 32: []byte("PLAYSTATION")}), SysPS1},
	{"ps1 iso", rom(0x10000, map[int][]byte{16*2048 + 1: []byte("CD001"), 16*2048 + 8: []byte("PLAYSTATION")}), SysPS1},
	{"zeros", rom(0x10000, nil), SysNone},
	{"text", []byte("just some notes about the games\n"), SysNone},
}

func TestSniff(t *testing.T) {
	for _, c := range sniffCases {
		if got := sniff(c.data, int64(len(c.data))); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func writeZip(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, data := range files {
		w, _ := zw.Create(name)
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, data []byte) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Platformers/Blue Hedgehog (USA).md", []byte("x"))
	write("Platformers/Blue Hedgehog (USA).png", []byte("x"))
	write("Platformers/readme.txt", []byte("x"))
	write("Platformers/mystery.bin", rom(0x10000, map[int][]byte{0x100: []byte("SEGA")}))
	write("Platformers/junk.bin", rom(0x200, nil))
	write("Docs/manual.pdf", []byte("x"))
	write("Puzzle/Deep/blocks.gb", []byte("x"))
	write("bios/gba_bios.bin", rom(0x4000, nil))
	write("zelda_like.sfc", []byte("x"))
	write("Disc Game.cue", []byte("FILE \"Disc Game.bin\" BINARY\n"))
	write("Disc Game.bin", rom(0x10000, map[int][]byte{0: cdSync, 16*2352 + 25: []byte("CD001"), 16*2352 + 32: []byte("PLAYSTATION")}))
	write("Kart Racer (USA).z64", rom(0x1000, map[int][]byte{0: {0x80, 0x37, 0x12, 0x40}}))
	write("Puzzle/Twin (USA).nes", []byte("x"))
	write("Puzzle/Twin (Japan).nes", []byte("x"))
	writeZip(t, filepath.Join(root, "sf2.zip"), map[string][]byte{"sf2e.30g": {1}, "sf2e.37g": {2}})
	writeZip(t, filepath.Join(root, "neogeo.zip"), map[string][]byte{"sp-s2.sp1": {1}, "000-lo.lo": {2}})
	writeZip(t, filepath.Join(root, "Zipped Game (Europe).zip"), map[string][]byte{"game.nes": []byte("NES\x1a")})
	writeZip(t, filepath.Join(root, "My Holiday Photos.zip"), map[string][]byte{"a.jpg": {1}, "b.jpg": {2}})

	top := Scan(root)
	var got []string
	var walk func(e *Entry, prefix string)
	walk = func(e *Entry, prefix string) {
		for _, c := range e.Children {
			line := prefix + c.Title
			if c.IsDir() {
				line += "/"
			} else {
				line += " [" + c.System.String() + "]"
				if c.Shot != "" {
					line += " +shot"
				}
			}
			got = append(got, line)
			walk(c, prefix+"  ")
		}
	}
	walk(top, "")
	want := []string{
		"Platformers/",
		"  Blue Hedgehog [Mega Drive] +shot",
		"  mystery [Mega Drive]",
		"Puzzle/",
		"  Deep/",
		"    blocks [Game Boy]",
		"  Twin (Japan) [NES]",
		"  Twin (USA) [NES]",
		"Disc Game [PlayStation]",
		"Kart Racer [Nintendo 64]",
		"sf2 [Arcade]",
		"zelda like [Super Nintendo]",
		"Zipped Game [NES]",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d:\n%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %q, want %q", i, got[i], want[i])
		}
	}
	if n := top.Count(); n != 10 {
		t.Errorf("Count: got %d, want 10", n)
	}
}
