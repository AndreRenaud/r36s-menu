package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/hajimehoshi/ebiten/v2/text/v2"
	"github.com/hajimehoshi/ebiten/v2/vector"
)

// The look follows the "Lo-fi Bedroom · Cassette Shelf" mockup (960×540),
// laid out again for the R36S's 640×480 panel: a window on the left showing
// the selected game's screenshot (or a rainy night when it has none), and
// the games as a curved shelf of cassettes on the right.

const (
	screenW = 640
	screenH = 480

	barH = 26 // status bar

	// Window: frame and the pane inside it
	frameX, frameY, frameW, frameH = 14, 40, 380, 302
	frameB                         = 10
	paneX, paneY                   = frameX + frameB, frameY + frameB
	paneW, paneH                   = frameW - 2*frameB, frameH - 2*frameB

	shelfY = frameY + frameH

	// Cassette list
	listX     = 412
	listY     = 82
	tapeW     = 186
	tapeH     = 38
	tapePitch = 45
	slots     = 7
	bulge     = 24.0 // how far the middle of the list curves out
	tilt      = 2.0  // degrees between neighbouring cassettes
	selShift  = 12.0
)

var (
	colBG      = color.RGBA{0x1b, 0x16, 0x26, 0xff}
	colDot     = color.RGBA{0x2a, 0x23, 0x3b, 0xff}
	colText    = color.RGBA{0xf6, 0xee, 0xfc, 0xff}
	colDim     = color.RGBA{0xc6, 0xb9, 0xd8, 0xff}
	colNote    = color.RGBA{0xd6, 0xcb, 0xe4, 0xff}
	colAccent  = color.RGBA{0xff, 0xb3, 0x8a, 0xff}
	colFrame   = color.RGBA{0x3a, 0x2f, 0x4f, 0xff}
	colShelf   = color.RGBA{0x4a, 0x3b, 0x60, 0xff}
	colCity    = color.RGBA{0x12, 0x0e, 0x1c, 0xff}
	colLit     = color.RGBA{0xff, 0xcf, 0x8a, 0xff}
	colTape    = color.RGBA{0x2b, 0x22, 0x38, 0xff}
	colReel    = color.RGBA{0x8a, 0x78, 0xa8, 0xff}
	colInk     = color.RGBA{0x2a, 0x1d, 0x33, 0xff}
	colPot     = color.RGBA{0x5a, 0x4a, 0x8a, 0xff}
	colLeaf    = color.RGBA{0x8f, 0xd6, 0xa3, 0xff}
	colWarn    = color.RGBA{0xff, 0x8a, 0x8a, 0xff}
	colCaption = color.RGBA{0x12, 0x0e, 0x1c, 0xa8}
)

// A cassette's label colour and the sky behind the window, as in the mockup.
type palette struct{ tape, skyTop, skyBot, moon color.RGBA }

func rgb(v uint32) color.RGBA {
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
}

var palettes = []palette{
	{rgb(0xf2d7b6), rgb(0x2b2050), rgb(0xb97a8f), rgb(0xffe3c2)},
	{rgb(0xcfe3d4), rgb(0x0f182b), rgb(0x3e5a7a), rgb(0xd9e6ff)},
	{rgb(0xd9e8b8), rgb(0x1f2b3a), rgb(0x6f8f6a), rgb(0xf5f0c8)},
	{rgb(0xffc9b3), rgb(0x2a1838), rgb(0xe08a6b), rgb(0xffd7a8)},
	{rgb(0xe9dcff), rgb(0x120f22), rgb(0x3b2f63), rgb(0xfff4b0)},
	{rgb(0xbfe1f2), rgb(0x1b2340), rgb(0x8a6fb3), rgb(0xffe6f0)},
	{rgb(0xf6e3a1), rgb(0x231a2c), rgb(0xc9837a), rgb(0xfff0d6)},
}

func paletteFor(e *Entry) palette {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(e.Name)))
	return palettes[h.Sum32()%uint32(len(palettes))]
}

//go:embed assets/Caveat.ttf
var caveatTTF []byte

//go:embed assets/Nunito.ttf
var nunitoTTF []byte

var caveatSrc, nunitoSrc *text.GoTextFaceSource

func face(src *text.GoTextFaceSource, size, weight float64) *text.GoTextFace {
	f := &text.GoTextFace{Source: src, Size: size}
	f.SetVariation(text.MustParseTag("wght"), float32(weight))
	return f
}

func drawText(dst *ebiten.Image, s string, f text.Face, x, y float64, c color.Color) {
	op := &text.DrawOptions{}
	op.GeoM.Translate(x, y)
	op.ColorScale.ScaleWithColor(c)
	text.Draw(dst, s, f, op)
}

func textWidth(s string, f text.Face) float64 {
	w, _ := text.Measure(s, f, 0)
	return w
}

// fit shortens s with an ellipsis until it is no wider than w.
func fit(s string, f text.Face, w float64) string {
	if textWidth(s, f) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 1 {
		r = r[:len(r)-1]
		if t := strings.TrimSpace(string(r)) + "…"; textWidth(t, f) <= w {
			return t
		}
	}
	return s
}

func roundRect(dst *ebiten.Image, x, y, w, h, r float32, c color.Color) {
	var p vector.Path
	p.MoveTo(x+r, y)
	p.LineTo(x+w-r, y)
	p.ArcTo(x+w, y, x+w, y+r, r)
	p.LineTo(x+w, y+h-r)
	p.ArcTo(x+w, y+h, x+w-r, y+h, r)
	p.LineTo(x+r, y+h)
	p.ArcTo(x, y+h, x, y+h-r, r)
	p.LineTo(x, y+r)
	p.ArcTo(x, y, x+r, y, r)
	p.Close()
	op := &vector.DrawPathOptions{AntiAlias: true}
	op.ColorScale.ScaleWithColor(c)
	vector.FillPath(dst, &p, nil, op)
}

type game struct {
	root *Entry
	dir  *Entry
	sel  int

	status   Status
	statusAt time.Time
	tick     int
	hold     map[string]int
	reload   chan os.Signal // the supervisor says the card has new pictures
	snd      *sounds
	volume   int // 0-10, or -1 when unknown

	// Starting a game: the picture grows to fill the screen first
	launch     *Entry
	launchTick int
	lastFrame  []byte // RGBA
	waitFrame  []byte
	stickOK    map[ebiten.GamepadID]bool
	quit       bool
	shotWanted string // -screenshot

	// Pictures that only change with the selection or the list
	bg       *ebiten.Image
	rain     *ebiten.Image
	reel     *ebiten.Image
	tapes    map[*Entry]*ebiten.Image
	scene    *ebiten.Image // the window pane without the rain
	sceneFor *Entry
	scroll   titleScroll

	// Folder change animation
	trans, transDir int
	fromDir         *Entry
	fromSel         int
	shake           int
	layers          [2]*ebiten.Image
	sceneShot       bool
	shots           map[string]*ebiten.Image
	shotLRU         []string

	fTitle, fHand, fHandSmall, fTape, fBig, fNote, fSmall, fBold *text.GoTextFace
}

func runUI() error {
	var err error
	if caveatSrc, err = text.NewGoTextFaceSource(bytes.NewReader(caveatTTF)); err != nil {
		return err
	}
	if nunitoSrc, err = text.NewGoTextFaceSource(bytes.NewReader(nunitoTTF)); err != nil {
		return err
	}
	g := &game{
		reload:     make(chan os.Signal, 1),
		hold:       map[string]int{},
		stickOK:    map[ebiten.GamepadID]bool{},
		tapes:      map[*Entry]*ebiten.Image{},
		shots:      map[string]*ebiten.Image{},
		shotWanted: *shotFile,
		fTitle:     face(caveatSrc, 28, 700),
		fHand:      face(caveatSrc, 19, 500),
		fHandSmall: face(caveatSrc, 17, 500),
		fTape:      face(caveatSrc, 20, 700),
		fBig:       face(nunitoSrc, 22, 800),
		fNote:      face(nunitoSrc, 12.5, 400),
		fSmall:     face(nunitoSrc, 12, 400),
		fBold:      face(nunitoSrc, 12, 800),
	}
	if st := loadState(); st.Launch != "" {
		// The choice that ended the last run has been acted on
		st.Launch = ""
		saveState(st)
	}
	g.refreshStatus()
	g.rescan()
	g.volume = readVolume(*volFile)
	if !*noSound && *shotFile == "" {
		g.snd = newSounds()
	}
	signal.Notify(g.reload, syscall.SIGUSR1)
	ebiten.SetWindowSize(screenW, screenH)
	ebiten.SetWindowTitle("R36S menu")
	ebiten.SetFullscreen(true)
	ebiten.SetCursorMode(ebiten.CursorModeHidden)
	err = ebiten.RunGame(g)
	if errors.Is(err, ebiten.Termination) {
		return nil
	}
	return err
}

func (g *game) Layout(int, int) (int, int) { return screenW, screenH }

func (g *game) refreshStatus() {
	g.status = ReadStatus(*powerDir, *romDir)
	g.statusAt = time.Now()
}

// rescan reads the card again and goes back to where the menu was.
func (g *game) rescan() {
	g.root = Scan(*romDir)
	if !g.status.CardPresent {
		g.root.Children = nil
	}
	g.dir, g.sel = g.root, 0
	g.trans, g.shake, g.fromDir = 0, 0, nil
	g.scroll.entry = nil
	for _, img := range g.tapes {
		img.Deallocate()
	}
	g.tapes = map[*Entry]*ebiten.Image{}
	g.sceneFor = nil

	st := loadState()
	if rel, err := filepath.Rel(*romDir, st.Dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			found := false
			for _, c := range g.dir.Children {
				if c.IsDir() && c.Name == part {
					g.dir, found = c, true
					break
				}
			}
			if !found {
				break
			}
		}
	}
	for i, c := range g.dir.Children {
		if c.Name == st.Name {
			g.sel = i
		}
	}
}

func (g *game) current() *Entry {
	if g.sel < len(g.dir.Children) {
		return g.dir.Children[g.sel]
	}
	return nil
}

// pressed reports a new press of an action, repeating while it is held.
func (g *game) pressed(name string, down bool, repeat bool) bool {
	if !down {
		g.hold[name] = 0
		return false
	}
	g.hold[name]++
	n := g.hold[name]
	return n == 1 || repeat && n > 24 && (n-24)%5 == 0
}

func (g *game) Update() error {
	g.tick++
	g.scroll.tick++
	select {
	case <-g.reload:
		// Stay where we are: rescan goes back to the saved position
		st := State{Dir: g.dir.Path}
		if e := g.current(); e != nil {
			st.Name = e.Name
		}
		saveState(st)
		for path, img := range g.shots {
			if img != nil {
				img.Deallocate()
			}
			delete(g.shots, path)
		}
		g.shotLRU = nil
		g.rescan()
	default:
	}
	if time.Since(g.statusAt) > 2*time.Second {
		had := g.status.CardPresent
		g.refreshStatus()
		if had != g.status.CardPresent {
			g.rescan()
		}
	}

	if g.tick%6 == 0 {
		g.volume = readVolume(*volFile)
	}
	if g.launch != nil {
		// Zooming into the picture: then hand over to the emulator
		g.launchTick++
		if g.launchTick >= launchTicks {
			if g.lastFrame == nil && g.launchTick < launchTicks+30 {
				return nil // Draw hasn't captured the last frame yet
			}
			// Leave the picture on the Linux framebuffer: that is what shows
			// from here until the emulator draws
			writeFramebuffer(*fbDev, g.lastFrame)
			// And leave the supervisor the picture to put there once the
			// game is up, for the moment between the game and the menu
			if fb := framebufferBytes(*fbDev, g.waitFrame); fb != nil {
				if os.WriteFile(*waitFile+".tmp", fb, 0o644) == nil {
					_ = os.Rename(*waitFile+".tmp", *waitFile)
				}
			}
			e := g.launch
			saveState(State{Dir: g.dir.Path, Name: e.Name, Launch: e.Path, System: e.System})
			return ebiten.Termination
		}
		return nil
	}

	var up, down, left, right, ok, back bool
	up = ebiten.IsKeyPressed(ebiten.KeyArrowUp)
	down = ebiten.IsKeyPressed(ebiten.KeyArrowDown)
	left = ebiten.IsKeyPressed(ebiten.KeyArrowLeft)
	right = ebiten.IsKeyPressed(ebiten.KeyArrowRight)
	ok = ebiten.IsKeyPressed(ebiten.KeyEnter) || ebiten.IsKeyPressed(ebiten.KeySpace)
	back = ebiten.IsKeyPressed(ebiten.KeyBackspace)
	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) {
		return ebiten.Termination
	}
	for _, id := range ebiten.AppendGamepadIDs(nil) {
		if os.Getenv("R36S_MENU_DEBUG") != "" && g.tick%30 == 0 {
			fmt.Fprintf(os.Stderr, "pad %d %q std=%v", id, ebiten.GamepadName(id), ebiten.IsStandardGamepadLayoutAvailable(id))
			for a := ebiten.StandardGamepadAxis(0); a <= ebiten.StandardGamepadAxisMax; a++ {
				fmt.Fprintf(os.Stderr, " ax%d=%.2f", a, ebiten.StandardGamepadAxisValue(id, a))
			}
			for b := ebiten.StandardGamepadButton(0); b <= ebiten.StandardGamepadButtonMax; b++ {
				if ebiten.IsStandardGamepadButtonPressed(id, b) {
					fmt.Fprintf(os.Stderr, " btn%d", b)
				}
			}
			fmt.Fprintln(os.Stderr)
		}
		if !ebiten.IsStandardGamepadLayoutAvailable(id) {
			continue
		}
		b := func(btn ebiten.StandardGamepadButton) bool { return ebiten.IsStandardGamepadButtonPressed(id, btn) }
		// A stick only counts once it has been seen at rest: one that
		// reads hard over from the start is broken or uncalibrated.
		y := ebiten.StandardGamepadAxisValue(id, ebiten.StandardGamepadAxisLeftStickVertical)
		if math.Abs(y) < 0.3 {
			g.stickOK[id] = true
		}
		if !g.stickOK[id] {
			y = 0
		}
		up = up || b(ebiten.StandardGamepadButtonLeftTop) || y < -0.6
		down = down || b(ebiten.StandardGamepadButtonLeftBottom) || y > 0.6
		left = left || b(ebiten.StandardGamepadButtonLeftLeft) || b(ebiten.StandardGamepadButtonFrontTopLeft)
		right = right || b(ebiten.StandardGamepadButtonLeftRight) || b(ebiten.StandardGamepadButtonFrontTopRight)
		// The R36S's buttons are labelled like Nintendo's: A is the right one, B the bottom one
		ok = ok || b(ebiten.StandardGamepadButtonRightRight)
		back = back || b(ebiten.StandardGamepadButtonRightBottom)
	}

	for _, p := range strings.Split(*pressList, ",") {
		var at int
		var action string
		if n, _ := fmt.Sscanf(p, "%d:%s", &at, &action); n == 2 && at == g.tick {
			up, down = up || action == "up", down || action == "down"
			left, right = left || action == "left", right || action == "right"
			ok, back = ok || action == "ok", back || action == "back"
		}
	}
	if g.shake > 0 {
		g.shake--
	}
	if g.trans > 0 {
		// Mid-slide: let it finish before taking more input
		g.trans--
		up, down, left, right, ok, back = false, false, false, false, false, false
	}

	selBefore, dirBefore := g.sel, g.dir
	defer func() {
		if g.dir == dirBefore && g.sel != selBefore {
			g.tickSound()
		}
	}()
	n := len(g.dir.Children)
	move := func(d int) {
		if n > 0 {
			g.sel = min(max(g.sel+d, 0), n-1)
		}
	}
	if g.pressed("up", up, true) {
		if g.sel == 0 && g.hold["up"] == 1 {
			g.sel = max(n-1, 0)
		} else {
			move(-1)
		}
	}
	if g.pressed("down", down, true) {
		if g.sel == n-1 && g.hold["down"] == 1 {
			g.sel = 0
		} else {
			move(1)
		}
	}
	if g.pressed("left", left, true) {
		move(-slots)
	}
	if g.pressed("right", right, true) {
		move(slots)
	}
	if g.pressed("ok", ok, false) {
		if e := g.current(); e != nil {
			if e.IsDir() {
				g.goTo(e, 0, 1)
				g.tickSound()
			} else {
				g.launch, g.launchTick = e, 0
				g.tickSound()
			}
		}
	}
	if g.pressed("back", back, false) {
		if g.dir.Parent == nil {
			// Nowhere to go back to: shudder
			g.shake = shakeTicks
		} else {
			sel := 0
			for i, c := range g.dir.Parent.Children {
				if c == g.dir {
					sel = i
				}
			}
			g.goTo(g.dir.Parent, sel, -1)
			g.tickSound()
		}
	}
	if g.quit {
		return ebiten.Termination
	}
	return nil
}

func (g *game) tickSound() {
	if g.snd != nil {
		g.snd.Tick()
	}
}

const launchTicks = 34 // length of the zoom into a game

// drawLaunch grows the selected game's picture from the window to the whole
// screen, as everything else goes dark.
func (g *game) drawLaunch(screen *ebiten.Image) {
	p := math.Min(1, float64(g.launchTick)/launchTicks)
	e := p * p * (3 - 2*p) // ease in and out
	vector.FillRect(screen, 0, 0, screenW, screenH, color.RGBA{0, 0, 0, uint8(255 * math.Min(1, e*1.3))}, false)

	// The screenshot itself if there is one, else the window's night view
	img := g.scene
	x0, y0, w0, h0 := float64(paneX), float64(paneY), float64(paneW), float64(paneH)
	if g.launch.Shot != "" {
		if shot := g.loadShot(g.launch.Shot); shot != nil {
			img = shot
			b := shot.Bounds()
			sc := math.Min(float64(paneW)/float64(b.Dx()), float64(paneH)/float64(b.Dy()))
			w0, h0 = float64(b.Dx())*sc, float64(b.Dy())*sc
			x0, y0 = paneX+(paneW-w0)/2, paneY+(paneH-h0)/2
		}
	}
	if img == nil {
		return
	}
	b := img.Bounds()
	// End filling the screen the way the emulator will: whole picture, in proportion
	sc := math.Min(screenW/float64(b.Dx()), screenH/float64(b.Dy()))
	w1, h1 := float64(b.Dx())*sc, float64(b.Dy())*sc
	x1, y1 := (screenW-w1)/2, (screenH-h1)/2
	w, h := w0+(w1-w0)*e, h0+(h1-h0)*e
	op := &ebiten.DrawImageOptions{Filter: ebiten.FilterLinear}
	op.GeoM.Scale(w/float64(b.Dx()), h/float64(b.Dy()))
	op.GeoM.Translate(x0+(x1-x0)*e, y0+(y1-y0)*e)
	screen.DrawImage(img, op)
}

func (g *game) Draw(screen *ebiten.Image) {
	g.drawBackground(screen)
	g.drawStatusBar(screen)
	g.drawWindow(screen)
	g.drawShelf(screen)
	g.drawList(screen)
	g.drawHints(screen)
	if g.launch != nil {
		g.drawLaunch(screen)
		if g.launchTick >= launchTicks && g.lastFrame == nil {
			g.lastFrame = make([]byte, 4*screenW*screenH)
			screen.ReadPixels(g.lastFrame)
			g.waitFrame = g.renderWait()
		}
	}

	if g.shotWanted != "" && g.tick >= *shotTick {
		img := image.NewRGBA(image.Rect(0, 0, screenW, screenH))
		screen.ReadPixels(img.Pix)
		if f, err := os.Create(g.shotWanted); err == nil {
			_ = png.Encode(f, img)
			f.Close()
		}
		g.shotWanted = ""
		g.quit = true
	}
}

func (g *game) drawBackground(screen *ebiten.Image) {
	if g.bg == nil {
		g.bg = ebiten.NewImage(screenW, screenH)
		g.bg.Fill(colBG)
		for y := 8; y < screenH; y += 18 {
			for x := 8; x < screenW; x += 18 {
				vector.FillCircle(g.bg, float32(x), float32(y), 1.2, colDot, true)
			}
		}
	}
	screen.DrawImage(g.bg, nil)
}

func (g *game) drawStatusBar(screen *ebiten.Image) {
	s := g.status

	// Battery, at the right: outline, level, and a bolt while on external power
	x := float32(screenW - 16)
	if s.HasBattery {
		label := fmt.Sprintf("%d%%", s.Percent)
		w := float32(textWidth(label, g.fBold))
		x -= w
		c := color.Color(colText)
		if s.Percent <= 15 && !s.Plugged {
			c = colWarn
		}
		drawText(screen, label, g.fBold, float64(x), 6, c)
		x -= 6 + 26
		vector.StrokeRect(screen, x, 8, 22, 11, 1.5, colDim, true)
		vector.FillRect(screen, x+22.5, 11, 2.5, 5, colDim, true)
		fill := colLeaf
		if s.Percent <= 15 {
			fill = colWarn
		} else if s.Percent <= 35 {
			fill = colAccent
		}
		vector.FillRect(screen, x+2, 10, 18*float32(s.Percent)/100, 7, fill, true)
		if s.Plugged {
			x -= 13
			var p vector.Path
			p.MoveTo(x+6, 6)
			p.LineTo(x+1, 14)
			p.LineTo(x+5, 14)
			p.LineTo(x+3, 21)
			p.LineTo(x+9, 12)
			p.LineTo(x+5, 12)
			p.Close()
			op := &vector.DrawPathOptions{AntiAlias: true}
			op.ColorScale.ScaleWithColor(colAccent)
			vector.FillPath(screen, &p, nil, op)
		}
		x -= 16
	}

	// Network: three linked boxes, shown while an adapter has a link
	if s.Network {
		x -= 14
		vector.FillRect(screen, x+4, 7, 6, 5, colDim, true)
		vector.FillRect(screen, x, 15, 5, 5, colDim, true)
		vector.FillRect(screen, x+9, 15, 5, 5, colDim, true)
		vector.FillRect(screen, x+6.4, 12, 1.2, 2, colDim, true)
		vector.FillRect(screen, x+2, 13.2, 10, 1.2, colDim, true)
		vector.FillRect(screen, x+2, 13.2, 1.2, 2, colDim, true)
		vector.FillRect(screen, x+10.8, 13.2, 1.2, 2, colDim, true)
		x -= 14
	}

	// Volume: a speaker and ten steps, or a cross when silent
	if g.volume >= 0 {
		const stepW, bars = 4, volumeMax
		x -= bars * stepW
		for i := 0; i < bars; i++ {
			c := colFrame
			if i < g.volume {
				c = colDim
			}
			h := 3 + float32(i)*0.9
			vector.FillRect(screen, x+float32(i)*stepW, 19-h, stepW-1.5, h, c, true)
		}
		x -= 6 + 12
		var p vector.Path
		p.MoveTo(x, 11)
		p.LineTo(x+4, 11)
		p.LineTo(x+9, 7)
		p.LineTo(x+9, 20)
		p.LineTo(x+4, 16)
		p.LineTo(x, 16)
		p.Close()
		op := &vector.DrawPathOptions{AntiAlias: true}
		op.ColorScale.ScaleWithColor(colDim)
		vector.FillPath(screen, &p, nil, op)
		if g.volume == 0 {
			vector.StrokeLine(screen, x-1, 20, x+12, 7, 1.8, colWarn, true)
		}
		x -= 16
	}

	// ROM card, at the left: a small card shape, a usage bar and "used / size"
	x = 16
	var p vector.Path
	p.MoveTo(x, 7)
	p.LineTo(x+7, 7)
	p.LineTo(x+10, 10)
	p.LineTo(x+10, 20)
	p.LineTo(x, 20)
	p.Close()
	op := &vector.DrawPathOptions{AntiAlias: true}
	op.ColorScale.ScaleWithColor(colDim)
	vector.FillPath(screen, &p, nil, op)
	x += 10 + 7
	label := "no card"
	if s.CardPresent && s.CardTotal > 0 {
		vector.FillRect(screen, x, 11, 44, 5, colFrame, true)
		used := float32(float64(s.CardUsed) / float64(s.CardTotal))
		vector.FillRect(screen, x, 11, max(44*used, 1), 5, colAccent, true)
		x += 44 + 7
		label = formatBytes(s.CardUsed) + " / " + formatBytes(s.CardTotal)
	}
	drawText(screen, label, g.fSmall, float64(x), 6, colDim)
}

// loadShot returns a screenshot, from a small cache.
func (g *game) loadShot(path string) *ebiten.Image {
	if img, ok := g.shots[path]; ok {
		return img
	}
	var img *ebiten.Image
	if f, err := os.Open(path); err == nil {
		if src, _, err := image.Decode(f); err == nil && src.Bounds().Dx() <= 4096 && src.Bounds().Dy() <= 4096 {
			img = ebiten.NewImageFromImage(src)
		}
		f.Close()
	}
	g.shots[path] = img
	g.shotLRU = append(g.shotLRU, path)
	if len(g.shotLRU) > 12 {
		old := g.shotLRU[0]
		g.shotLRU = g.shotLRU[1:]
		if o := g.shots[old]; o != nil {
			o.Deallocate()
		}
		delete(g.shots, old)
	}
	return img
}

// renderScene draws the window pane for an entry: its screenshot over the
// sky, or the whole rainy-night view when there is none.
func (g *game) renderScene(e *Entry) (hasShot bool) {
	if g.scene == nil {
		g.scene = ebiten.NewImage(paneW, paneH)
	}
	pal := palettes[4]
	if e != nil {
		pal = paletteFor(e)
	}
	dst := g.scene
	// Sky: a vertical gradient
	const bands = 56
	for i := 0; i < bands; i++ {
		t := float64(i) / (bands - 1)
		mix := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }
		c := color.RGBA{mix(pal.skyTop.R, pal.skyBot.R), mix(pal.skyTop.G, pal.skyBot.G), mix(pal.skyTop.B, pal.skyBot.B), 0xff}
		vector.FillRect(dst, 0, float32(i)*paneH/bands, paneW, paneH/bands+1, c, false)
	}
	if e != nil && e.Shot != "" {
		if img := g.loadShot(e.Shot); img != nil {
			b := img.Bounds()
			sc := math.Min(float64(paneW)/float64(b.Dx()), float64(paneH)/float64(b.Dy()))
			op := &ebiten.DrawImageOptions{Filter: ebiten.FilterLinear}
			if sc >= 2 && sc == math.Floor(sc) {
				op.Filter = ebiten.FilterNearest
			}
			op.GeoM.Scale(sc, sc)
			op.GeoM.Translate((paneW-float64(b.Dx())*sc)/2, (paneH-float64(b.Dy())*sc)/2)
			dst.DrawImage(img, op)
			return true
		}
	}
	// Moon with a glow
	mx, my := float32(paneW)*0.52+24, float32(56)
	for i := 6; i >= 1; i-- {
		c := pal.moon
		a := 0.05
		c = color.RGBA{uint8(float64(c.R) * a), uint8(float64(c.G) * a), uint8(float64(c.B) * a), uint8(255 * a)}
		vector.FillCircle(dst, mx, my, 24+float32(i)*6, c, true)
	}
	vector.FillCircle(dst, mx, my, 24, pal.moon, true)
	// Skyline, as fractions of the pane (the mockup's clip-path)
	sky := [][2]float32{{0, 60}, {8, 60}, {8, 32}, {16, 32}, {16, 50}, {24, 50}, {24, 14}, {31, 14}, {31, 44}, {40, 44}, {40, 26},
		{52, 26}, {52, 56}, {60, 56}, {60, 8}, {67, 8}, {67, 40}, {76, 40}, {76, 30}, {86, 30}, {86, 54}, {100, 54}, {100, 100}, {0, 100}}
	const cityH = 126
	var p vector.Path
	for i, pt := range sky {
		x, y := pt[0]*paneW/100, paneH-cityH+pt[1]*cityH/100
		if i == 0 {
			p.MoveTo(x, y)
		} else {
			p.LineTo(x, y)
		}
	}
	p.Close()
	op := &vector.DrawPathOptions{}
	op.ColorScale.ScaleWithColor(colCity)
	vector.FillPath(dst, &p, nil, op)
	return false
}

// Lit windows in the skyline: x as a fraction of the pane, y from the top,
// and whether (and when) it flickers.
var litWindows = []struct {
	x, y  float32
	phase float64
}{{0.26, 184, 0}, {0.62, 170, 1.2}, {0.63, 188, -1}, {0.44, 200, 2}, {0.70, 214, -1}}

func (g *game) drawWindow(screen *ebiten.Image) {
	e := g.current()
	if g.sceneFor != e || g.scene == nil {
		g.sceneFor = e
		g.sceneShot = g.renderScene(e)
	}
	hasShot := g.sceneShot
	roundRect(screen, frameX, frameY, frameW, frameH, 16, colFrame)
	screen.DrawImage(g.scene, geo(paneX, paneY))

	pane := screen.SubImage(image.Rect(paneX, paneY, paneX+paneW, paneY+paneH)).(*ebiten.Image)
	if !hasShot {
		secs := float64(g.tick) / 60
		for _, w := range litWindows {
			a := 1.0
			if w.phase >= 0 {
				a = 0.675 + 0.325*math.Cos(2*math.Pi*(secs+w.phase)/3.5)
			}
			c := color.RGBA{uint8(float64(colLit.R) * a), uint8(float64(colLit.G) * a), uint8(float64(colLit.B) * a), uint8(255 * a)}
			vector.FillRect(pane, paneX+w.x*paneW, paneY+w.y, 6, 8, c, false)
		}
		// Rain: one tile of slanted streaks, sliding down and to the left
		if g.rain == nil {
			g.rain = ebiten.NewImage(40, 160)
			for i := -4; i < 4; i++ {
				x0 := float32(i)*15.5 + 20
				vector.StrokeLine(g.rain, x0+43, 0, x0, 160, 1, color.RGBA{0x33, 0x33, 0x33, 0x33}, true)
			}
		}
		phase := math.Mod(float64(g.tick)/60/0.55, 1)
		ox, oy := -40*phase, 160*phase
		for y := oy - 160; y < paneH; y += 160 {
			for x := ox; x < paneW; x += 40 {
				pane.DrawImage(g.rain, geo(paneX+x, paneY+y))
			}
		}
		// The window's glazing bars
		vector.FillRect(pane, paneX+paneW/2-4, paneY, 8, paneH, colFrame, false)
		vector.FillRect(pane, paneX, paneY+paneH/2-4, paneW, 8, colFrame, false)
	}

	// Caption
	head, title, note := "now spinning…", "", ""
	switch {
	case e == nil && !g.status.CardPresent:
		head, title, note = "nothing to play…", "No ROM card", "Put a card with ROMs in the second SD slot"
	case e == nil:
		head, title, note = "nothing to play…", "No ROMs found", "Copy ROMs to the card, or over SFTP to ~/roms"
	case e.IsDir():
		head, title = "a box of tapes…", e.Title
		note = fmt.Sprintf("%d games", e.Count())
		if e.Count() == 1 {
			note = "1 game"
		}
	default:
		title = e.Title
		note = e.System.String() + " · " + formatBytes(uint64(e.Size))
	}
	const padX, maxW = 12, paneW - 24 - 24 - 84
	title = fit(title, g.fBig, maxW)
	note = fit(note, g.fNote, maxW)
	w := math.Max(math.Max(textWidth(title, g.fBig), textWidth(note, g.fNote)), textWidth(head, g.fHand)) + 2*padX
	const boxH = 74
	bx, by := float32(paneX+12), float32(paneY+paneH-12-boxH)
	roundRect(screen, bx, by, float32(w), boxH, 10, colCaption)
	drawText(screen, head, g.fHand, float64(bx)+padX, float64(by)+5, colAccent)
	drawText(screen, title, g.fBig, float64(bx)+padX, float64(by)+24, colText)
	drawText(screen, note, g.fNote, float64(bx)+padX, float64(by)+52, colNote)
}

func geo(x, y float64) *ebiten.DrawImageOptions {
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Translate(x, y)
	return op
}

func (g *game) drawShelf(screen *ebiten.Image) {
	const y = shelfY
	roundRect(screen, frameX-8, y, frameW+16, 12, 3, colShelf)
	// Plant
	roundRect(screen, 346, y-22, 26, 22, 4, colPot)
	vector.FillCircle(screen, 359, y-30, 14, colLeaf, true)
	vector.FillCircle(screen, 350, y-27, 9, colLeaf, true)
	vector.FillCircle(screen, 368, y-27, 9, colLeaf, true)
	// Mug with steam
	roundRect(screen, 296, y-19, 22, 19, 5, colAccent)
	vector.StrokeCircle(screen, 320, y-10, 5, 2.5, colAccent, true)
	t := math.Mod(float64(g.tick)/60/2.6, 1)
	a := math.Sin(t*math.Pi) * 0.5
	c := color.RGBA{uint8(246 * a), uint8(238 * a), uint8(252 * a), uint8(255 * a)}
	roundRect(screen, 305-float32(t)*1.5, y-36-float32(t)*14+4, 5+float32(t)*2, 13, 2.5, c)

	// Under the shelf: where we are on the card
	if e := g.current(); e != nil {
		path := "/"
		if rel, err := filepath.Rel(*romDir, g.dir.Path); err == nil && rel != "." {
			path = "/" + rel
		}
		drawText(screen, fit("on the card: "+path, g.fHandSmall, frameW), g.fHandSmall, frameX, y+22, colDim)
		drawText(screen, fit(e.Name, g.fSmall, frameW), g.fSmall, frameX, y+46, colReel)
	}
}

// tapeImage draws one cassette (without its reels' spin or the selection
// ring) once and keeps it.
func (g *game) tapeImage(e *Entry) *ebiten.Image {
	if img, ok := g.tapes[e]; ok {
		return img
	}
	img := g.renderTape(e, true)
	g.tapes[e] = img
	if len(g.tapes) > 64 {
		for k, v := range g.tapes {
			if k != e {
				v.Deallocate()
				delete(g.tapes, k)
				break
			}
		}
	}
	return img
}

// tapeLabel is where a cassette's title goes: its text and the left edge
// and width of the space for it.
func tapeLabel(e *Entry) (title string, x, w float64) {
	if e.IsDir() {
		return e.Title + "  ›", 30 + 7, tapeW - 38 - 14
	}
	return e.Title, 30 + 7, tapeW - 60 - 14
}

func (g *game) renderTape(e *Entry, withTitle bool) *ebiten.Image {
	img := ebiten.NewImage(tapeW, tapeH)
	roundRect(img, 0, 0, tapeW, tapeH, 7, colTape)
	label := paletteFor(e).tape
	title, tx, tw := tapeLabel(e)
	if e.IsDir() {
		// A box of tapes: a wider label and a stack mark instead of reels
		label = color.RGBA{0xc6, 0xb9, 0xd8, 0xff}
		for i := 0; i < 3; i++ {
			roundRect(img, 8, 10+float32(i)*7, 14, 4, 1.5, colReel)
		}
	}
	roundRect(img, float32(tx)-7, 6, float32(tw)+14, tapeH-12, 3, label)
	if withTitle {
		drawText(img, fit(title, g.fTape, tw), g.fTape, tx, 6, colInk)
	}
	return img
}

// A title too long for its cassette slides back and forth while the
// cassette is selected, so that all of it can be read.
const (
	scrollPause = 50  // ticks at each end
	scrollSpeed = 0.5 // pixels a tick
)

type titleScroll struct {
	entry *Entry
	tick  int
	blank *ebiten.Image // the cassette without its title
	strip *ebiten.Image // the whole title
	over  float64       // how much wider the title is than its space
}

// scrollFor returns the sliding title for the selected entry, or nil if its
// title fits.
func (g *game) scrollFor(e *Entry) *titleScroll {
	sc := &g.scroll
	if sc.entry != e {
		if sc.blank != nil {
			sc.blank.Deallocate()
			sc.strip.Deallocate()
			sc.blank, sc.strip = nil, nil
		}
		sc.entry, sc.tick, sc.over = e, 0, 0
		title, _, tw := tapeLabel(e)
		if w := textWidth(title, g.fTape); w > tw {
			sc.over = w - tw
			sc.blank = g.renderTape(e, false)
			sc.strip = ebiten.NewImage(int(math.Ceil(w))+2, tapeH)
			drawText(sc.strip, title, g.fTape, 0, 6, colInk)
		}
	}
	if sc.blank == nil {
		return nil
	}
	return sc
}

// offset is how far the title has slid: rest, slide to the end, rest, slide
// back.
func (sc *titleScroll) offset() float64 {
	travel := int(sc.over / scrollSpeed)
	t := sc.tick % (2*scrollPause + 2*travel)
	switch {
	case t < scrollPause:
		return 0
	case t < scrollPause+travel:
		return float64(t-scrollPause) * scrollSpeed
	case t < 2*scrollPause+travel:
		return sc.over
	}
	return sc.over - float64(t-2*scrollPause-travel)*scrollSpeed
}

func (g *game) reelImage() *ebiten.Image {
	if g.reel == nil {
		g.reel = ebiten.NewImage(18, 18)
		for i := 0; i < 8; i++ {
			a := float64(i) * math.Pi / 4
			vector.FillCircle(g.reel, 9+float32(math.Cos(a))*6.5, 9+float32(math.Sin(a))*6.5, 1.9, colReel, true)
		}
	}
	return g.reel
}

// drawListAt draws a folder's header and cassettes, moved by (ox, oy).
func (g *game) drawListAt(dst *ebiten.Image, dir *Entry, sel int, ox, oy float64) {
	title := dir.Title
	n := len(dir.Children)
	count := fmt.Sprintf("%d games", dir.Count())
	if dir.Count() == 1 {
		count = "1 game"
	}
	cw := textWidth(count, g.fSmall)
	drawText(dst, fit(title, g.fTitle, screenW-16-listX-cw-8), g.fTitle, listX+ox, 34+oy, colText)
	drawText(dst, count, g.fSmall, screenW-16-cw+ox, 50+oy, colDim)

	top := min(max(sel-slots/2, 0), max(n-slots, 0))
	const mid = float64(slots-1) / 2
	for slot := 0; slot < slots && top+slot < n; slot++ {
		i := top + slot
		e := dir.Children[i]
		on := i == sel
		d := (float64(slot) - mid) / mid
		x := listX + ox + bulge*(1-d*d)
		if on {
			x += selShift
		}
		y := float64(listY+slot*tapePitch) + oy
		rot := (float64(slot) - mid) * tilt * math.Pi / 180

		// Everything on a cassette turns about the middle of its left edge
		place := func(op *ebiten.DrawImageOptions, lx, ly float64) {
			op.GeoM.Translate(lx, ly-tapeH/2)
			op.GeoM.Rotate(rot)
			op.GeoM.Translate(x, y+tapeH/2)
			op.Filter = ebiten.FilterLinear
		}
		if on {
			ring := g.ringImage()
			op := &ebiten.DrawImageOptions{}
			place(op, -8, -8)
			dst.DrawImage(ring, op)
		}
		var sc *titleScroll
		if on && dir == g.dir {
			sc = g.scrollFor(e)
		}
		op := &ebiten.DrawImageOptions{}
		place(op, 0, 0)
		if sc == nil {
			dst.DrawImage(g.tapeImage(e), op)
		} else {
			dst.DrawImage(sc.blank, op)
			// The part of the title that is in view
			_, tx, tw := tapeLabel(e)
			off := sc.offset()
			view := sc.strip.SubImage(image.Rect(int(off), 0, int(off)+int(tw), tapeH)).(*ebiten.Image)
			op := &ebiten.DrawImageOptions{}
			place(op, tx, 0)
			dst.DrawImage(view, op)
		}
		if !e.IsDir() {
			for _, rx := range []float64{15, tapeW - 15} {
				op := &ebiten.DrawImageOptions{}
				op.GeoM.Translate(-9, -9)
				if on {
					op.GeoM.Rotate(2 * math.Pi * math.Mod(float64(g.tick)/60/2.2, 1))
				}
				place(op, rx, tapeH/2)
				dst.DrawImage(g.reelImage(), op)
			}
		}
	}
	// More above or below
	arrow := func(y float32, dir float32) {
		var p vector.Path
		x := float32(screenW-24) + float32(ox)
		y += float32(oy)
		p.MoveTo(x-5, y)
		p.LineTo(x+5, y)
		p.LineTo(x, y+7*dir)
		p.Close()
		op := &vector.DrawPathOptions{AntiAlias: true}
		op.ColorScale.ScaleWithColor(colReel)
		vector.FillPath(dst, &p, nil, op)
	}
	if top > 0 {
		arrow(listY-6, -1)
	}
	if top+slots < n {
		arrow(float32(listY+slots*tapePitch), 1)
	}
}

// The list sits in this part of the screen; it is drawn through a layer
// while one folder's list slides out and another's slides in.
const (
	layerX, layerY = 396, 28
	layerW, layerH = screenW - layerX, 422

	transTicks = 16 // length of the slide between folders
	shakeTicks = 22 // length of the "can't go back" shudder
	slideDist  = 90.0
)

func (g *game) drawList(screen *ebiten.Image) {
	if g.trans == 0 {
		ox := 0.0
		if g.shake > 0 {
			// A decaying wobble
			t := float64(shakeTicks-g.shake) / shakeTicks
			ox = 9 * math.Sin(t*math.Pi*5) * (1 - t)
		}
		g.drawListAt(screen, g.dir, g.sel, ox, 0)
		return
	}
	if g.layers[0] == nil {
		g.layers[0] = ebiten.NewImage(layerW, layerH)
		g.layers[1] = ebiten.NewImage(layerW, layerH)
	}
	p := 1 - float64(g.trans)/transTicks
	e := 1 - math.Pow(1-p, 3) // ease out
	// Into a folder (transDir 1): the old list leaves to the left and the
	// new one comes in from the right. Back out of one: the reverse.
	d := float64(g.transDir)
	// The lists stay in their column: nothing slides across the window
	column := screen.SubImage(image.Rect(layerX, 0, screenW, screenH)).(*ebiten.Image)
	layer := func(img *ebiten.Image, dir *Entry, sel int, x, alpha float64) {
		img.Clear()
		g.drawListAt(img, dir, sel, -layerX, -layerY)
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Translate(layerX+x, layerY)
		op.ColorScale.ScaleAlpha(float32(alpha))
		column.DrawImage(img, op)
	}
	layer(g.layers[0], g.fromDir, g.fromSel, -d*e*slideDist, 1-e)
	layer(g.layers[1], g.dir, g.sel, d*(1-e)*slideDist, e)
}

// goTo changes folder with the slide: dirn 1 into a folder, -1 back out.
func (g *game) goTo(dir *Entry, sel int, dirn int) {
	g.fromDir, g.fromSel = g.dir, g.sel
	g.dir, g.sel = dir, sel
	g.trans, g.transDir = transTicks, dirn
}

var ringImg *ebiten.Image

// ringImage is the selected cassette's outline with a soft glow around it.
func (g *game) ringImage() *ebiten.Image {
	if ringImg == nil {
		ringImg = ebiten.NewImage(tapeW+16, tapeH+16)
		for k := 8; k >= 3; k-- {
			a := 0.035
			c := color.RGBA{uint8(255 * a), uint8(179 * a), uint8(138 * a), uint8(255 * a)}
			roundRect(ringImg, 8-float32(k), 8-float32(k), tapeW+2*float32(k), tapeH+2*float32(k), 7+float32(k), c)
		}
		roundRect(ringImg, 6, 6, tapeW+4, tapeH+4, 9, colAccent)
	}
	return ringImg
}

func (g *game) drawHints(screen *ebiten.Image) {
	x, y := float64(listX), float64(screenH-30)
	hint := func(btn, label string, filled bool) {
		if filled {
			vector.FillCircle(screen, float32(x)+10, float32(y)+10, 10, colAccent, true)
			drawText(screen, btn, g.fBold, x+10-textWidth(btn, g.fBold)/2, y+2, color.RGBA{0x2a, 0x14, 0x08, 0xff})
		} else {
			vector.StrokeCircle(screen, float32(x)+10, float32(y)+10, 9, 1.5, colDim, true)
			drawText(screen, btn, g.fBold, x+10-textWidth(btn, g.fBold)/2, y+2, colDim)
		}
		drawText(screen, label, g.fSmall, x+26, y+2, colDim)
		x += 26 + textWidth(label, g.fSmall) + 16
	}
	e := g.current()
	switch {
	case e == nil:
	case e.IsDir():
		hint("A", "Open", true)
	default:
		hint("A", "Play", true)
	}
	if g.dir.Parent != nil {
		hint("B", "Back", false)
	}
}

// renderWait draws what shows between a game ending and the menu coming
// back: the menu's backdrop and a line of text.
func (g *game) renderWait() []byte {
	img := ebiten.NewImage(screenW, screenH)
	defer img.Deallocate()
	g.drawBackground(img)
	const line1, line2 = "rewinding the tape…", "back to the shelf in a moment"
	big := face(caveatSrc, 44, 700)
	drawText(img, line1, big, (screenW-textWidth(line1, big))/2, 180, colAccent)
	drawText(img, line2, g.fNote, (screenW-textWidth(line2, g.fNote))/2, 244, colDim)
	out := make([]byte, 4*screenW*screenH)
	img.ReadPixels(out)
	return out
}

// framebufferBytes converts a screen-sized RGBA picture to the Linux
// framebuffer's layout (32 bits a pixel, as the R36S's is; nil for anything
// else).
func framebufferBytes(dev string, rgba []byte) []byte {
	if rgba == nil {
		return nil
	}
	sys := filepath.Join("/sys/class/graphics", filepath.Base(dev))
	var w, h, stride int
	if n, _ := fmt.Sscanf(readTrim(filepath.Join(sys, "virtual_size")), "%d,%d", &w, &h); n != 2 {
		return nil
	}
	fmt.Sscanf(readTrim(filepath.Join(sys, "stride")), "%d", &stride)
	if readTrim(filepath.Join(sys, "bits_per_pixel")) != "32" || w < screenW || h < screenH || stride < 4*w {
		return nil
	}
	out := make([]byte, stride*screenH)
	for y := 0; y < screenH; y++ {
		src, dst := rgba[y*4*screenW:], out[y*stride:]
		for x := 0; x < screenW; x++ {
			dst[4*x], dst[4*x+1], dst[4*x+2], dst[4*x+3] = src[4*x+2], src[4*x+1], src[4*x], 0xff
		}
	}
	return out
}

// writeFramebuffer puts a screen-sized RGBA picture on the Linux framebuffer.
func writeFramebuffer(dev string, rgba []byte) {
	if out := framebufferBytes(dev, rgba); out != nil {
		if f, err := os.OpenFile(dev, os.O_WRONLY, 0); err == nil {
			f.Write(out)
			f.Close()
		}
	}
}
