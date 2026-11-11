package main

// A headless libretro frontend that makes a ROM's screenshot: it loads a
// core, runs the game for a while without display, sound or input, and
// writes the picture it ends on to a PNG. It runs as its own process
// (r36s-menu -snap ...), because a core can crash or hang on a bad ROM.
//
// The cores are C libraries. They are loaded and called with purego, as
// Ebitengine does for the graphics libraries, so the menu needs no cgo.

import (
	"archive/zip"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libretro.h
const (
	retroEnvExperimental       = 0x10000
	retroEnvGetCanDupe         = 3
	retroEnvGetSystemDirectory = 9
	retroEnvSetPixelFormat     = 10
	retroEnvGetSaveDirectory   = 31

	retroPixel0RGB1555 = 0
	retroPixelXRGB8888 = 1
	retroPixelRGB565   = 2
)

type retroGameInfo struct {
	path *byte
	data unsafe.Pointer
	size uintptr
	meta *byte
}

type retroSystemInfo struct {
	libraryName     *byte
	libraryVersion  *byte
	validExtensions *byte
	needFullpath    bool
	blockExtract    bool
}

type snapper struct {
	pixelFormat uint32
	sysDir      *byte
	saveDir     *byte

	// The most recent frame
	frame         []byte
	width, height int
	pitch         int
	frames        int
}

func cstring(s string) *byte {
	b := append([]byte(s), 0)
	return &b[0]
}

func (s *snapper) environment(cmd uint32, data unsafe.Pointer) uintptr {
	switch cmd &^ retroEnvExperimental {
	case retroEnvGetCanDupe:
		*(*bool)(data) = true
		return 1
	case retroEnvSetPixelFormat:
		s.pixelFormat = *(*uint32)(data)
		return 1
	case retroEnvGetSystemDirectory:
		*(**byte)(data) = s.sysDir
		return 1
	case retroEnvGetSaveDirectory:
		*(**byte)(data) = s.saveDir
		return 1
	}
	// Everything else (core options, logging, rumble, ...): not supported,
	// which cores must cope with
	return 0
}

func (s *snapper) videoRefresh(data unsafe.Pointer, width, height uint32, pitch uintptr) {
	s.frames++
	if data == nil || width == 0 || height == 0 || width > 4096 || height > 4096 {
		return // a duplicated frame: the last one stands
	}
	n := int(pitch) * int(height)
	if cap(s.frame) < n {
		s.frame = make([]byte, n)
	}
	s.frame = s.frame[:n]
	copy(s.frame, unsafe.Slice((*byte)(data), n))
	s.width, s.height, s.pitch = int(width), int(height), int(pitch)
}

// image converts the last frame.
func (s *snapper) image() *image.RGBA {
	if s.width == 0 {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, s.width, s.height))
	for y := 0; y < s.height; y++ {
		row := s.frame[y*s.pitch:]
		out := img.Pix[y*img.Stride:]
		for x := 0; x < s.width; x++ {
			var r, g, b uint8
			switch s.pixelFormat {
			case retroPixelXRGB8888:
				b, g, r = row[x*4], row[x*4+1], row[x*4+2]
			case retroPixelRGB565:
				v := uint16(row[x*2]) | uint16(row[x*2+1])<<8
				r, g, b = uint8(v>>11)<<3, uint8(v>>5&0x3f)<<2, uint8(v&0x1f)<<3
				r, g, b = r|r>>5, g|g>>6, b|b>>5
			default: // 0RGB1555
				v := uint16(row[x*2]) | uint16(row[x*2+1])<<8
				r, g, b = uint8(v>>10&0x1f)<<3, uint8(v>>5&0x1f)<<3, uint8(v&0x1f)<<3
				r, g, b = r|r>>5, g|g>>5, b|b>>5
			}
			out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] = r, g, b, 0xff
		}
	}
	return img
}

// blank reports whether a picture is (nearly) one flat colour, as between
// a game's logos.
func blank(img *image.RGBA) bool {
	if img == nil {
		return true
	}
	counts := map[[3]uint8]int{}
	total := 0
	for y := 0; y < img.Rect.Dy(); y += 2 {
		for x := 0; x < img.Rect.Dx(); x += 2 {
			p := img.Pix[y*img.Stride+x*4:]
			counts[[3]uint8{p[0] >> 3, p[1] >> 3, p[2] >> 3}]++
			total++
		}
	}
	most := 0
	for _, n := range counts {
		most = max(most, n)
	}
	return most*100 >= total*97
}

// romData reads the ROM for a core that wants it in memory. A zip holding
// one console ROM gives that ROM.
func romData(path string) ([]byte, error) {
	if strings.EqualFold(filepath.Ext(path), ".zip") {
		zr, err := zip.OpenReader(path)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || f.UncompressedSize64 > 64<<20 {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
		return nil, errors.New("empty zip")
	}
	return os.ReadFile(path)
}

// runSnap is "r36s-menu -snap": run rom in core for the given number of
// frames, carry on (for up to as many again) while the picture is blank, and
// write it to out.
func runSnap(core, rom, out string, frames int, sys System) error {
	// The core calls back on the thread it is called from
	runtime.LockOSThread()

	work, err := os.MkdirTemp("", "r36s-snap")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	s := &snapper{sysDir: cstring(filepath.Join(filepath.Dir(rom), "bios")), saveDir: cstring(work)}
	if _, err := os.Stat(filepath.Join(*romDir, "bios")); err == nil {
		s.sysDir = cstring(filepath.Join(*romDir, "bios"))
	}

	lib, err := purego.Dlopen(core, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return err
	}
	var (
		setEnvironment, setVideoRefresh, setAudioSample, setAudioSampleBatch func(uintptr)
		setInputPoll, setInputState                                          func(uintptr)
		retroInit, retroDeinit, retroRun, retroUnloadGame                    func()
		getSystemInfo                                                        func(*retroSystemInfo)
		loadGame                                                             func(*retroGameInfo) bool
	)
	for name, fn := range map[string]any{
		"retro_set_environment": &setEnvironment, "retro_set_video_refresh": &setVideoRefresh,
		"retro_set_audio_sample": &setAudioSample, "retro_set_audio_sample_batch": &setAudioSampleBatch,
		"retro_set_input_poll": &setInputPoll, "retro_set_input_state": &setInputState,
		"retro_init": &retroInit, "retro_deinit": &retroDeinit, "retro_run": &retroRun,
		"retro_unload_game": &retroUnloadGame, "retro_get_system_info": &getSystemInfo, "retro_load_game": &loadGame,
	} {
		purego.RegisterLibFunc(fn, lib, name)
	}

	setEnvironment(purego.NewCallback(s.environment))
	setVideoRefresh(purego.NewCallback(s.videoRefresh))
	setAudioSample(purego.NewCallback(func(l, r int16) {}))
	setAudioSampleBatch(purego.NewCallback(func(data unsafe.Pointer, n uintptr) uintptr { return n }))
	setInputPoll(purego.NewCallback(func() {}))
	setInputState(purego.NewCallback(func(port, device, index, id uint32) uintptr { return 0 }))
	retroInit()

	var info retroSystemInfo
	getSystemInfo(&info)
	game := retroGameInfo{path: cstring(rom)}
	var data []byte
	if !info.needFullpath && sys != SysArcade {
		if data, err = romData(rom); err != nil {
			return err
		}
		if len(data) == 0 {
			return errors.New("empty ROM")
		}
		game.data, game.size = unsafe.Pointer(&data[0]), uintptr(len(data))
	}
	if !loadGame(&game) {
		return errors.New("the core didn't load it")
	}
	var img *image.RGBA
	for i := 0; i < 2*frames; i++ {
		retroRun()
		if i+1 >= frames && (i+1-frames)%30 == 0 {
			if img = s.image(); !blank(img) {
				break
			}
		}
	}
	runtime.KeepAlive(data)
	if img == nil || blank(img) {
		return fmt.Errorf("no picture after %d frames", s.frames)
	}
	f, err := os.Create(out + ".tmp")
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// No retro_unload_game or retro_deinit: the process ends here, and some
	// cores write files as they shut down
	return os.Rename(out+".tmp", out)
}
