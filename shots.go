package main

// Screenshots for ROMs that have none are made in the background while the
// menu is showing: one ROM at a time, at idle priority, each in its own
// process (see snap.go). The pictures collect in a folder on the system
// card. The ROM card stays mounted read-only; once enough pictures are
// waiting (or there are no more to make) it is remounted read-write just
// long enough to copy them next to their ROMs. A card that already has all
// its pictures is never written to.

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	shotBatch     = 10               // pictures to collect before writing them to the card
	shotTimeout   = 90 * time.Second // for one ROM
	shotMaxUsed   = 90               // % of the card: above this, no pictures are made
	shotMaxBytes  = 4 << 20          // a picture bigger than this isn't copied
	shotIdleSleep = time.Minute      // between looks at a card with nothing to do
)

type shotWorker struct {
	cacheDir string              // pending pictures and the list of failures
	cred     *syscall.Credential // who the snapshot processes run as (nil: as we are)
	env      []string
	notify   func() // called after pictures were written to the card

	mu     sync.Mutex
	paused bool
	child  *exec.Cmd
	wake   chan struct{}
}

func newShotWorker(cacheDir string, cred *syscall.Credential, env []string, notify func()) *shotWorker {
	w := &shotWorker{cacheDir: cacheDir, cred: cred, env: env, notify: notify, wake: make(chan struct{}, 1)}
	go w.loop()
	return w
}

// Pause stops the work at once (a game is starting); Resume lets it carry on.
func (w *shotWorker) Pause() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.paused = true
	if w.child != nil && w.child.Process != nil {
		_ = syscall.Kill(-w.child.Process.Pid, syscall.SIGKILL)
	}
}

func (w *shotWorker) Resume() {
	w.mu.Lock()
	w.paused = false
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *shotWorker) isPaused() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paused
}

func (w *shotWorker) sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-w.wake:
	}
}

func (w *shotWorker) pendingDir() string { return filepath.Join(w.cacheDir, "pending") }

// key names a ROM's pending picture.
func shotKey(rom string) string {
	h := sha1.Sum([]byte(rom))
	return hex.EncodeToString(h[:8])
}

// pending lists the waiting pictures: key -> ROM path.
func (w *shotWorker) pending() map[string]string {
	out := map[string]string{}
	names, _ := filepath.Glob(filepath.Join(w.pendingDir(), "*.rom"))
	for _, n := range names {
		key := strings.TrimSuffix(filepath.Base(n), ".rom")
		rom, err := os.ReadFile(n)
		if _, perr := os.Lstat(filepath.Join(w.pendingDir(), key+".png")); err != nil || perr != nil {
			continue
		}
		out[key] = string(rom)
	}
	return out
}

func (w *shotWorker) failedFile() string { return filepath.Join(w.cacheDir, "failed.txt") }

// failed is the ROMs that gave no picture, so they aren't tried again and
// again: "path\tsize" lines.
func (w *shotWorker) failed() map[string]bool {
	out := map[string]bool{}
	b, _ := os.ReadFile(w.failedFile())
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" {
			out[line] = true
		}
	}
	return out
}

func failKey(e *Entry) string { return e.Path + "\t" + formatBytes(uint64(e.Size)) }

// Systems whose cores draw with the GPU, which the headless runner can't do.
var noSnap = map[System]bool{SysN64: true}

func (w *shotWorker) loop() {
	for {
		if w.isPaused() {
			w.sleep(time.Hour)
			continue
		}
		st := ReadStatus(*powerDir, *romDir)
		if !st.CardPresent || st.CardTotal == 0 || st.CardUsed*100 > st.CardTotal*shotMaxUsed {
			w.sleep(shotIdleSleep)
			continue
		}
		pending, failed := w.pending(), w.failed()
		var todo []*Entry
		var walk func(e *Entry)
		walk = func(e *Entry) {
			for _, c := range e.Children {
				if c.IsDir() {
					walk(c)
				} else if c.Shot == "" && settled(c.Path) && !noSnap[c.System] && findCore(c.System) != "" && !failed[failKey(c)] && pending[shotKey(c.Path)] == "" {
					todo = append(todo, c)
				}
			}
		}
		walk(Scan(*romDir))

		if len(pending) >= shotBatch || len(pending) > 0 && len(todo) == 0 {
			w.flush(pending)
			continue
		}
		if len(todo) == 0 {
			w.sleep(shotIdleSleep)
			continue
		}
		// One ROM, then look again: the card may have changed meanwhile
		w.snap(todo[0])
		w.sleep(500 * time.Millisecond)
	}
}

// snap makes one ROM's picture in a child process.
func (w *shotWorker) snap(e *Entry) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	key := shotKey(e.Path)
	out := filepath.Join(w.pendingDir(), key+".png")
	cmd := exec.Command(self, "-roms", *romDir, "-cores", *coreDir, "-snap", e.Path, "-snap-out", out)
	cmd.Env = w.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: w.cred}

	w.mu.Lock()
	if w.paused {
		w.mu.Unlock()
		return
	}
	if err := cmd.Start(); err != nil {
		w.mu.Unlock()
		log.Printf("screenshots: %v", err)
		w.sleep(shotIdleSleep)
		return
	}
	w.child = cmd
	w.mu.Unlock()
	lowPriority(cmd.Process.Pid)

	timer := time.AfterFunc(shotTimeout, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	err = cmd.Wait()
	timer.Stop()
	w.mu.Lock()
	w.child = nil
	paused := w.paused
	w.mu.Unlock()

	switch {
	case paused:
		// Killed for a game: try this ROM again later
		os.Remove(out)
	case err != nil:
		os.Remove(out)
		if f, ferr := os.OpenFile(w.failedFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ferr == nil {
			f.WriteString(failKey(e) + "\n")
			f.Close()
		}
	default:
		_ = os.WriteFile(filepath.Join(w.pendingDir(), key+".rom"), []byte(e.Path), 0o644)
	}
}

// lowPriority makes a process run only when nothing else wants the CPU.
func lowPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
	const schedIdle = 5
	var param struct{ priority int32 }
	_, _, _ = syscall.Syscall(syscall.SYS_SCHED_SETSCHEDULER, uintptr(pid), schedIdle, uintptr(unsafe.Pointer(&param)))
}

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// flush copies the waiting pictures onto the card, next to their ROMs, with
// the card read-write only for as long as that takes.
func (w *shotWorker) flush(pending map[string]string) {
	clean := func(key string) {
		os.Remove(filepath.Join(w.pendingDir(), key+".png"))
		os.Remove(filepath.Join(w.pendingDir(), key+".rom"))
	}
	root := filepath.Clean(*romDir) + string(filepath.Separator)
	type job struct{ key, src, dst string }
	var jobs []job
	for key, rom := range pending {
		dst := strings.TrimSuffix(rom, filepath.Ext(rom)) + ".png"
		_, romErr := os.Stat(rom)
		_, dstErr := os.Lstat(dst)
		if !strings.HasPrefix(filepath.Clean(rom), root) || romErr != nil || dstErr == nil {
			clean(key) // the ROM has gone, or it has a picture now
			continue
		}
		jobs = append(jobs, job{key, filepath.Join(w.pendingDir(), key+".png"), dst})
	}
	if len(jobs) == 0 {
		return
	}
	// Someone may have made the card writable to copy ROMs onto it: then it
	// is left that way
	wasReadOnly := isReadOnly(*romDir)
	if wasReadOnly {
		if err := remount(*romDir, false); err != nil {
			log.Printf("screenshots: can't make %s writable: %v", *romDir, err)
			w.sleep(10 * shotIdleSleep)
			return
		}
	}
	written := 0
	for _, j := range jobs {
		if err := copyPicture(j.src, j.dst); err != nil {
			log.Printf("screenshots: %s: %v", j.dst, err)
		} else {
			written++
		}
		clean(j.key)
	}
	syscall.Sync()
	if wasReadOnly {
		if err := remount(*romDir, true); err != nil {
			log.Printf("screenshots: can't make %s read-only again: %v", *romDir, err)
		}
	}
	log.Printf("screenshots: wrote %d to the card", written)
	if written > 0 && w.notify != nil {
		w.notify()
	}
}

// copyPicture copies a pending picture to the card. The picture was written
// by an unprivileged process running a core on an arbitrary ROM, so it is
// only taken if it is a plain PNG file of a sensible size, and it never
// replaces a file.
func copyPicture(src, dst string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > shotMaxBytes {
		return os.ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(in, shotMaxBytes))
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, pngMagic) {
		return os.ErrInvalid
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err = out.Write(data); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// remount switches the card between read-only and read-write, keeping its
// other mount options.
func remount(dir string, readOnly bool) error {
	flags := uintptr(syscall.MS_REMOUNT | syscall.MS_NOATIME)
	if readOnly {
		flags |= syscall.MS_RDONLY
	}
	return syscall.Mount("", dir, "", flags, "")
}

func isReadOnly(dir string) bool {
	var fs syscall.Statfs_t
	return syscall.Statfs(dir, &fs) == nil && fs.Flags&1 != 0 // ST_RDONLY
}

// settled reports whether a file has been left alone for a while: one that
// is still being copied onto the card isn't ready to run.
func settled(path string) bool {
	st, err := os.Stat(path)
	return err == nil && time.Since(st.ModTime()) > 2*time.Minute
}
