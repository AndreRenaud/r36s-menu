package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// System is an emulated machine. The zero value means "not a ROM".
type System int

const (
	SysNone System = iota
	SysGB
	SysGBC
	SysGBA
	SysNES
	SysSNES
	SysMegaDrive
	SysMasterSystem
	SysGameGear
	SysArcade
	SysN64
	SysPS1
)

var systemNames = map[System]string{
	SysGB:           "Game Boy",
	SysGBC:          "Game Boy Color",
	SysGBA:          "Game Boy Advance",
	SysNES:          "NES",
	SysSNES:         "Super Nintendo",
	SysMegaDrive:    "Mega Drive",
	SysMasterSystem: "Master System",
	SysGameGear:     "Game Gear",
	SysArcade:       "Arcade",
	SysN64:          "Nintendo 64",
	SysPS1:          "PlayStation",
}

func (s System) String() string { return systemNames[s] }

// Extensions that settle the system by themselves.
var extSystems = map[string]System{
	".gb":  SysGB,
	".sgb": SysGB,
	".gbc": SysGBC,
	".gba": SysGBA,
	".nes": SysNES,
	".sfc": SysSNES,
	".smc": SysSNES,
	".md":  SysMegaDrive,
	".gen": SysMegaDrive,
	".smd": SysMegaDrive,
	".sms": SysMasterSystem,
	".gg":  SysGameGear,
	".z64": SysN64,
	".n64": SysN64,
	".v64": SysN64,
	".cue": SysPS1,
	".chd": SysPS1,
	".pbp": SysPS1,
}

// Extensions that need a look at the contents.
// .bin is anything from a Mega Drive cartridge to a PlayStation disc.
var sniffExts = map[string]bool{".bin": true, ".rom": true, ".img": true, ".iso": true}

// How much of a file sniff needs: enough for a HiROM SNES header behind a
// 512-byte copier header.
const sniffLen = 0x10000 + 0x200

// DetectFile works out which system a file is for, from its name and, where
// the name isn't enough, its contents. Folder names play no part: the card's
// folders are the user's own. Files that aren't ROMs give SysNone.
func DetectFile(path string) System {
	ext := strings.ToLower(filepath.Ext(path))
	if s, ok := extSystems[ext]; ok {
		return s
	}
	switch {
	case ext == ".zip":
		return detectZip(path)
	case sniffExts[ext]:
		f, err := os.Open(path)
		if err != nil {
			return SysNone
		}
		defer f.Close()
		var size int64
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
		buf := make([]byte, sniffLen)
		n, _ := io.ReadFull(f, buf)
		return sniff(buf[:n], size)
	}
	return SysNone
}

// detectZip looks inside a zip. One holding a single console ROM is that
// ROM (RetroArch's cores load those directly). Anything else that looks like
// a set of chip dumps is an arcade ROM set, which the arcade cores identify
// by the zip's name.
func detectZip(path string) System {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return SysNone
	}
	defer zr.Close()
	var files []*zip.File
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return SysNone
	}
	if len(files) == 1 {
		f := files[0]
		ext := strings.ToLower(filepath.Ext(f.Name))
		if s, ok := extSystems[ext]; ok {
			return s
		}
		if sniffExts[ext] {
			if rc, err := f.Open(); err == nil {
				buf := make([]byte, sniffLen)
				n, _ := io.ReadFull(rc, buf)
				rc.Close()
				if s := sniff(buf[:n], int64(f.UncompressedSize64)); s != SysNone {
					return s
				}
			}
		}
	}
	// Arcade sets: short, lower-case names without spaces (sf2.zip,
	// mslug.zip), holding ROM chip dumps rather than documents.
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if len(base) > 16 || strings.ContainsAny(base, " ()[]") {
		return SysNone
	}
	for _, f := range files {
		switch strings.ToLower(filepath.Ext(f.Name)) {
		case ".exe", ".dll", ".doc", ".pdf", ".htm", ".html", ".jpg", ".jpeg", ".png", ".mp3", ".iso", ".cue":
			return SysNone
		}
	}
	return SysArcade
}

var nintendoLogoStart = []byte{0xCE, 0xED, 0x66, 0x66, 0xCC, 0x0D, 0x00, 0x0B}
var cdSync = []byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00}
var gbaLogoStart = []byte{0x24, 0xFF, 0xAE, 0x51, 0x69, 0x9A, 0xA2, 0x21}

// sniff identifies a ROM from its first bytes. size is the whole file's
// length.
func sniff(b []byte, size int64) System {
	has := func(off int, sig []byte) bool {
		return len(b) >= off+len(sig) && bytes.Equal(b[off:off+len(sig)], sig)
	}
	switch {
	case has(0, []byte("NES\x1a")):
		return SysNES
	case has(4, gbaLogoStart) && len(b) > 0xB2 && b[0xB2] == 0x96:
		return SysGBA
	case has(0x104, nintendoLogoStart):
		if b[0x143]&0x80 != 0 {
			return SysGBC
		}
		return SysGB
	case has(0x100, []byte("SEGA")):
		return SysMegaDrive
	case has(0, []byte{0x80, 0x37, 0x12, 0x40}), has(0, []byte{0x37, 0x80, 0x40, 0x12}), has(0, []byte{0x40, 0x12, 0x37, 0x80}):
		// The three byte orders N64 ROMs come in
		return SysN64
	case has(0, cdSync) && has(16*2352+24+1, []byte("CD001")) && has(16*2352+24+8, []byte("PLAYSTATION")):
		// A raw CD image (2352-byte sectors): the volume descriptor is in sector 16
		return SysPS1
	case has(16*2048+1, []byte("CD001")) && has(16*2048+8, []byte("PLAYSTATION")):
		return SysPS1
	}
	for _, off := range []int{0x7FF0, 0x3FF0, 0x1FF0} {
		if has(off, []byte("TMR SEGA")) {
			// The region nibble tells Game Gear from Master System
			if len(b) > off+0xF && b[off+0xF]>>4 >= 5 {
				return SysGameGear
			}
			return SysMasterSystem
		}
	}
	if isSNES(b, size) {
		return SysSNES
	}
	return SysNone
}

// isSNES looks for the SNES internal header (LoROM or HiROM, with or without
// a 512-byte copier header): the checksum and its complement add up to
// 0xFFFF, and the title is printable.
func isSNES(b []byte, size int64) bool {
	skip := 0
	if size%1024 == 512 {
		skip = 512
	}
	for _, base := range []int{0x7FC0, 0xFFC0} {
		h := base + skip
		if len(b) < h+0x20 {
			continue
		}
		sum := int(b[h+0x1E]) | int(b[h+0x1F])<<8
		comp := int(b[h+0x1C]) | int(b[h+0x1D])<<8
		if sum+comp != 0xFFFF {
			continue
		}
		ok := true
		for _, c := range b[h : h+21] {
			if c < 0x20 || c > 0x7E {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
