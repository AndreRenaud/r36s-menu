// r36s-menu is the R36S's ROM menu. It shows the ROMs on the card in the
// second SD slot and runs RetroArch with the right core for the chosen one.
//
// Only one program at a time can own the display, so the menu runs as two
// processes: a supervisor, which starts the menu itself (this program again,
// with -ui), waits for it to exit having written its choice to a file, runs
// the emulator, and starts the menu again.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	romDir       = flag.String("roms", "/roms", "folder with the ROMs (the ROM card's mount point)")
	coreDir      = flag.String("cores", "/usr/lib/libretro", "folder with the libretro cores")
	retroarch    = flag.String("retroarch", "retroarch", "RetroArch program")
	stateFile    = flag.String("state", "/tmp/r36s-menu.state", "file that remembers the menu position and carries the choice")
	powerDir     = flag.String("power", "/sys/class/power_supply", "power supply sysfs folder")
	uiMode       = flag.Bool("ui", false, "run the menu itself (the supervisor does this)")
	anyDir       = flag.Bool("any-dir", false, "show -roms even if nothing is mounted on it (for development)")
	shotFile     = flag.String("screenshot", "", "write the menu's picture to this PNG after a second and exit (for development)")
	shotTick     = flag.Int("screenshot-tick", 61, "with -screenshot: the tick (1/60 s) to take it at")
	snapROM      = flag.String("snap", "", "make this ROM's screenshot without using the display, and exit (the supervisor does this)")
	snapOut      = flag.String("snap-out", "", "with -snap: the PNG to write")
	snapLen      = flag.Int("snap-frames", 0, "with -snap: frames to run before taking the picture (default: 15 s of play, 25 s for arcade)")
	retroCfg     = flag.String("retroarch-config", "/etc/retroarch.cfg", "RetroArch configuration file")
	runUser      = flag.String("user", "", "when started as root: run the menu and the emulator as this user")
	noShots      = flag.Bool("no-screenshots", false, "don't make missing screenshots in the background")
	volFile      = flag.String("volume-file", "/tmp/r36s-menu.volume", "file the supervisor publishes the volume level in")
	padName      = flag.String("volume-keys", "R36S Gamepad", "name of the input device with the volume keys")
	powerKey     = flag.String("power-key", "rk805 pwrkey", "name of the input device with the power button")
	mixerControl = flag.String("mixer", "Master", "ALSA mixer control for the volume")
	noSound      = flag.Bool("no-sound", false, "no menu sounds")
	fbDev        = flag.String("framebuffer", "/dev/fb0", "Linux framebuffer that shows between the menu and the emulator")
	waitFile     = flag.String("wait-picture", "/tmp/r36s-menu.wait", "file the menu leaves its between-games picture in, as framebuffer data")
	systemDir    = flag.String("system-dir", "", "RetroArch's system folder: links to the card's bios folder are kept in it")
	pressList    = flag.String("press", "", "with -screenshot: presses to fake, as tick:action,... (actions: up down left right ok back)")
)

// Cores for each system, in order of preference.
var cores = map[System][]string{
	SysGB:           {"gambatte", "mgba"},
	SysGBC:          {"gambatte", "mgba"},
	SysGBA:          {"mgba"},
	SysNES:          {"fceumm"},
	SysSNES:         {"snes9x", "snes9x2005"},
	SysMegaDrive:    {"genesis_plus_gx", "picodrive"},
	SysMasterSystem: {"genesis_plus_gx", "picodrive"},
	SysGameGear:     {"genesis_plus_gx"},
	SysArcade:       {"fbneo", "mame2003_plus"},
	SysN64:          {"mupen64plus_next"},
	SysPS1:          {"pcsx_rearmed"},
}

// State is saved between runs of the menu.
type State struct {
	Dir    string `json:"dir"`    // folder the menu was showing
	Name   string `json:"name"`   // selected entry in it
	Launch string `json:"launch"` // ROM to run; set by the menu, cleared by the supervisor
	System System `json:"system"`
}

func loadState() State {
	var s State
	if b, err := os.ReadFile(*stateFile); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveState(s State) {
	b, _ := json.Marshal(s)
	tmp := *stateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, *stateFile)
	}
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("r36s-menu: ")
	flag.Parse()
	romDirIsTest = *anyDir
	if *snapROM != "" {
		sys := DetectFile(*snapROM)
		core := findCore(sys)
		if core == "" {
			log.Fatalf("no core for %s", *snapROM)
		}
		frames := *snapLen
		if frames == 0 {
			frames = 15 * 60
			if sys == SysArcade {
				frames = 25 * 60
			}
		}
		if err := runSnap(core, *snapROM, *snapOut, frames, sys); err != nil {
			log.Fatalf("%s: %v", *snapROM, err)
		}
		return
	}
	if *uiMode || *shotFile != "" {
		if err := runUI(); err != nil {
			log.Fatal(err)
		}
		return
	}
	supervise()
}

// childIdentity works out who the menu, the emulator and the screenshot
// maker run as. Started as root with -user, the supervisor stays root (it
// remounts the ROM card to write screenshots) and everything else runs as
// that user.
func childIdentity() (*syscall.Credential, []string) {
	env := os.Environ()
	if *runUser == "" || os.Getuid() != 0 {
		return nil, env
	}
	u, err := user.Lookup(*runUser)
	if err != nil {
		log.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if ids, err := u.GroupIds(); err == nil {
		for _, id := range ids {
			if n, err := strconv.Atoi(id); err == nil {
				cred.Groups = append(cred.Groups, uint32(n))
			}
		}
	}
	keep := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "USER=") && !strings.HasPrefix(kv, "LOGNAME=") {
			keep = append(keep, kv)
		}
	}
	env = append(keep, "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username)
	// Folders the user's programs expect, made as the user's
	for _, d := range []string{".cache/r36s-menu/pending", ".local/share", ".config"} {
		p := u.HomeDir
		for _, part := range strings.Split(d, "/") {
			p = filepath.Join(p, part)
			if os.Mkdir(p, 0o755) == nil {
				_ = os.Chown(p, uid, gid)
			}
		}
	}
	return cred, append(env, "R36S_MENU_HOME="+u.HomeDir)
}

// supervise alternates between the menu and the emulator, and makes missing
// screenshots in the background while the menu is up.
func supervise() {
	self, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	cred, env := childIdentity()
	home := os.Getenv("HOME")
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			home = kv[5:]
		}
	}

	var mu sync.Mutex
	var child *exec.Cmd
	var childIsMenu bool
	signalChild := func(sig syscall.Signal, group bool) {
		mu.Lock()
		defer mu.Unlock()
		if child != nil && child.Process != nil {
			pid := child.Process.Pid
			if group {
				pid = -pid
			}
			_ = syscall.Kill(pid, sig)
		}
	}

	startVolume(filepath.Join(home, ".config", "r36s-menu", "volume"), *volFile)
	// A short press of the power button leaves a game for the menu.
	// RetroArch saves the game's battery memory as it exits.
	go watchPower(*powerKey, func() {
		mu.Lock()
		game := child != nil && !childIsMenu
		mu.Unlock()
		if game {
			log.Printf("power button: back to the menu")
			signalChild(syscall.SIGTERM, false)
		}
	})

	var shots *shotWorker
	if !*noShots {
		cache := filepath.Join(home, ".cache", "r36s-menu")
		_ = os.MkdirAll(filepath.Join(cache, "pending"), 0o755)
		shots = newShotWorker(cache, cred, env, func() {
			// New pictures on the card: have the menu read it again
			mu.Lock()
			menu := childIsMenu
			mu.Unlock()
			if menu {
				signalChild(syscall.SIGUSR1, false)
			}
		})
	}

	// Asked to stop: start nothing more, end what is running, and go
	var stopping bool
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		mu.Lock()
		stopping = true
		mu.Unlock()
		if shots != nil {
			shots.Pause()
		}
		signalChild(syscall.SIGTERM, true)
		for i := 0; i < 20; i++ {
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			gone := child == nil
			mu.Unlock()
			if gone {
				os.Exit(0)
			}
		}
		signalChild(syscall.SIGKILL, true)
		os.Exit(0)
	}()
	run := func(menu bool, name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Env = env
		cmd.Dir = home
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
		mu.Lock()
		if stopping {
			mu.Unlock()
			select {} // the signal handler ends the process
		}
		if err := cmd.Start(); err != nil {
			mu.Unlock()
			return err
		}
		child, childIsMenu = cmd, menu
		mu.Unlock()
		err := cmd.Wait()
		mu.Lock()
		child = nil
		mu.Unlock()
		return err
	}

	for {
		started := time.Now()
		args := append([]string{"-ui"}, os.Args[1:]...)
		if err := run(true, self, args...); err != nil {
			log.Printf("menu: %v", err)
			if time.Since(started) < 5*time.Second {
				time.Sleep(2 * time.Second)
			}
			continue
		}
		// The menu clears a choice as it starts, so this one is new
		st := loadState()
		if st.Launch == "" {
			continue
		}
		core := findCore(st.System)
		if core == "" {
			log.Printf("no core installed for %v", st.System)
			continue
		}
		// The game gets the whole machine: stop making screenshots at once
		if shots != nil {
			shots.Pause()
		}
		linkBIOS(home)
		log.Printf("running %s with %s", st.Launch, filepath.Base(core))
		args = []string{"-L", core, st.Launch}
		if _, err := os.Stat(*retroCfg); err == nil {
			// Without --config RetroArch copies the system file to the
			// user's folder on its first run and uses that copy from then on
			args = append([]string{"--config", *retroCfg}, args...)
		}
		// Once the game has the display, change what is underneath it: the
		// framebuffer shows again when the game ends, and by then it should
		// say that the menu is coming, not look like the game loading
		go func(rom string) {
			time.Sleep(3 * time.Second)
			mu.Lock()
			game := child != nil && !childIsMenu
			mu.Unlock()
			if st := loadState(); !game || st.Launch != rom {
				return
			}
			showWaitPicture()
		}(st.Launch)
		if err := run(false, *retroarch, args...); err != nil {
			log.Printf("%s: %v", *retroarch, err)
		}
		if shots != nil {
			shots.Resume()
		}
	}
}

func findCore(sys System) string {
	for _, c := range cores[sys] {
		p := filepath.Join(*coreDir, c+"_libretro.so")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// linkBIOS fills RetroArch's system folder, which is on the system card
// because some cores write to it, with links to the BIOS files in the ROM
// card's bios folder.
func linkBIOS(home string) {
	dir := *systemDir
	if dir == "" {
		dir = filepath.Join(home, "system")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return
	}
	old, _ := os.ReadDir(dir)
	for _, e := range old {
		p := filepath.Join(dir, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			if _, err := os.Stat(p); err != nil {
				os.Remove(p) // its card has gone
			}
		}
	}
	bios := filepath.Join(*romDir, "bios")
	files, _ := os.ReadDir(bios)
	for _, e := range files {
		if !e.IsDir() {
			_ = os.Symlink(filepath.Join(bios, e.Name()), filepath.Join(dir, e.Name()))
		}
	}
}

// showWaitPicture copies the menu's between-games picture to the framebuffer.
// The file comes from the unprivileged menu, so only a plain file of exactly
// the framebuffer's size is used.
func showWaitPicture() {
	sys := filepath.Join("/sys/class/graphics", filepath.Base(*fbDev))
	var w, h, stride int
	fmt.Sscanf(readTrim(filepath.Join(sys, "virtual_size")), "%d,%d", &w, &h)
	fmt.Sscanf(readTrim(filepath.Join(sys, "stride")), "%d", &stride)
	in, err := os.OpenFile(*waitFile, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil || !st.Mode().IsRegular() || stride == 0 || st.Size() > int64(stride*h) || st.Size()%int64(stride) != 0 {
		return
	}
	data := make([]byte, st.Size())
	if _, err := io.ReadFull(in, data); err != nil {
		return
	}
	if fb, err := os.OpenFile(*fbDev, os.O_WRONLY, 0); err == nil {
		fb.Write(data)
		fb.Close()
	}
}
