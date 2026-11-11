package main

// The volume buttons on the side of the unit. The supervisor handles them,
// since it is there for both the menu and the emulator: it reads the keys
// straight from the input device, sets the sound card's volume, remembers
// the level, and publishes it in a file for the menu's status bar.

import (
	"encoding/binary"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	volumeMax     = 10
	volumeDefault = 3

	keyVolumeDown = 114
	keyVolumeUp   = 115
)

// volumeRaw is the RK817 "Master" control's value (0-255, 0.375 dB a step)
// for a level: 0 is silent, 1 is about -49 dB and 10 about -22 dB, which is
// already loud on the R36S's speaker.
func volumeRaw(level int) int {
	if level <= 0 {
		return 0
	}
	return 115 + 8*min(level, volumeMax)
}

type volumeControl struct {
	level    int
	saveFile string // remembered across restarts
	pubFile  string // read by the menu
	device   string // name of the input device with the volume keys
}

func startVolume(saveFile, pubFile string) {
	v := &volumeControl{level: volumeDefault, saveFile: saveFile, pubFile: pubFile, device: *padName}
	if b, err := os.ReadFile(saveFile); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			v.level = min(max(n, 0), volumeMax)
		}
	}
	v.apply()
	go v.watch()
}

func (v *volumeControl) apply() {
	if err := exec.Command("amixer", "-q", "sset", *mixerControl, strconv.Itoa(volumeRaw(v.level))).Run(); err != nil {
		log.Printf("volume: amixer: %v", err)
	}
	text := []byte(strconv.Itoa(v.level) + "\n")
	_ = os.MkdirAll(filepath.Dir(v.saveFile), 0o755)
	_ = os.WriteFile(v.saveFile, text, 0o644)
	if os.WriteFile(v.pubFile+".tmp", text, 0o644) == nil {
		// Readable by the menu whatever umask we were started with
		_ = os.Chmod(v.pubFile+".tmp", 0o644)
		_ = os.Rename(v.pubFile+".tmp", v.pubFile)
	}
}

// openInput finds an input device by name.
func openInput(device string) *os.File {
	paths, _ := filepath.Glob("/dev/input/event*")
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		var name [128]byte
		// EVIOCGNAME(len(name))
		req := uintptr(0x80000000 | len(name)<<16 | 'E'<<8 | 0x06)
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&name[0]))); errno == 0 {
			if strings.TrimRight(string(name[:]), "\x00") == device {
				return f
			}
		}
		f.Close()
	}
	return nil
}

func (v *volumeControl) watch() {
	for {
		f := openInput(v.device)
		if f == nil {
			time.Sleep(2 * time.Second)
			continue
		}
		v.read(f)
		f.Close()
		time.Sleep(time.Second)
	}
}

// read follows the keys until the device goes away. A held key repeats.
func (v *volumeControl) read(f *os.File) {
	events := make(chan [2]int, 16) // key code, value
	go func() {
		defer close(events)
		// struct input_event on 64-bit: two 8-byte time fields, type, code, value
		var ev [24]byte
		for {
			if _, err := f.Read(ev[:]); err != nil {
				return
			}
			typ := binary.LittleEndian.Uint16(ev[16:])
			code := int(binary.LittleEndian.Uint16(ev[18:]))
			value := int(int32(binary.LittleEndian.Uint32(ev[20:])))
			if typ == 1 && (code == keyVolumeUp || code == keyVolumeDown) {
				events <- [2]int{code, value}
			}
		}
	}()
	held := 0
	step := func() {
		d := 1
		if held == keyVolumeDown {
			d = -1
		}
		if n := min(max(v.level+d, 0), volumeMax); n != v.level {
			v.level = n
			v.apply()
		}
	}
	repeat := time.NewTimer(time.Hour)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch {
			case ev[1] == 1:
				held = ev[0]
				step()
				repeat.Reset(450 * time.Millisecond)
			case ev[1] == 0 && ev[0] == held:
				held = 0
			}
		case <-repeat.C:
			if held != 0 {
				step()
				repeat.Reset(160 * time.Millisecond)
			}
		}
	}
}

// readVolume is the menu's side: the published level, or -1 if there is none.
func readVolume(pubFile string) int {
	b, err := os.ReadFile(pubFile)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	return min(max(n, 0), volumeMax)
}

const keyPower = 116

// watchPower calls onShort for each short press of the power button (down
// and up within a second). A long press is the power controller's business:
// it switches the unit off by itself.
func watchPower(device string, onShort func()) {
	for {
		f := openInput(device)
		if f == nil {
			time.Sleep(2 * time.Second)
			continue
		}
		var down time.Time
		var ev [24]byte
		for {
			if _, err := f.Read(ev[:]); err != nil {
				break
			}
			typ := binary.LittleEndian.Uint16(ev[16:])
			code := binary.LittleEndian.Uint16(ev[18:])
			value := int32(binary.LittleEndian.Uint32(ev[20:]))
			if typ != 1 || code != keyPower {
				continue
			}
			switch {
			case value == 1:
				down = time.Now()
			case value == 0 && !down.IsZero():
				if time.Since(down) < time.Second {
					onShort()
				}
				down = time.Time{}
			}
		}
		f.Close()
		time.Sleep(time.Second)
	}
}
