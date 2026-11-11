package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Status is what the bar at the top shows.
type Status struct {
	HasBattery bool
	Percent    int
	Charging   bool // the battery is taking charge
	Plugged    bool // external power is connected

	Network bool // a wired network adapter is plugged in and has a link

	CardPresent bool
	CardTotal   uint64 // bytes
	CardUsed    uint64
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ReadStatus reads the battery and charger state from sysfs and the ROM
// card's size and usage. romDir counts as a card only while something is
// mounted on it.
func ReadStatus(powerDir, romDir string) Status {
	var s Status
	supplies, _ := filepath.Glob(filepath.Join(powerDir, "*"))
	for _, d := range supplies {
		switch readTrim(filepath.Join(d, "type")) {
		case "Battery":
			if n, err := strconv.Atoi(readTrim(filepath.Join(d, "capacity"))); err == nil {
				s.HasBattery = true
				s.Percent = min(max(n, 0), 100)
			}
			if readTrim(filepath.Join(d, "status")) == "Charging" {
				s.Charging = true
			}
		case "Mains", "USB":
			if readTrim(filepath.Join(d, "online")) == "1" {
				s.Plugged = true
			}
		}
	}
	if s.Charging {
		s.Plugged = true
	}

	// Network: any real interface (the USB Ethernet adapter) with a carrier
	nets, _ := filepath.Glob("/sys/class/net/*")
	for _, d := range nets {
		if filepath.Base(d) != "lo" && readTrim(filepath.Join(d, "carrier")) == "1" {
			s.Network = true
		}
	}

	var st, parent syscall.Stat_t
	if syscall.Stat(romDir, &st) == nil && syscall.Stat(filepath.Dir(filepath.Clean(romDir)), &parent) == nil {
		var fs syscall.Statfs_t
		if (st.Dev != parent.Dev || romDirIsTest) && syscall.Statfs(romDir, &fs) == nil {
			s.CardPresent = true
			s.CardTotal = fs.Blocks * uint64(fs.Bsize)
			s.CardUsed = (fs.Blocks - fs.Bfree) * uint64(fs.Bsize)
		}
	}
	return s
}

// romDirIsTest makes any folder count as a card (-any-dir, for development).
var romDirIsTest bool

func formatBytes(n uint64) string {
	const gb = 1 << 30
	switch {
	case n >= 10*gb:
		return strconv.FormatFloat(float64(n)/gb, 'f', 0, 64) + " GB"
	case n >= gb/10:
		return strconv.FormatFloat(float64(n)/gb, 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 0, 64) + " MB"
	default:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 0, 64) + " KB"
	}
}
