# r36s-menu

The ROM menu for the R36S handheld: it lists the ROMs on the card in the
second SD slot and runs RetroArch with the right libretro core for the one
you pick. Written in Go with [Ebitengine](https://ebitengine.org), drawing
straight to the display (GBM/KMS), 640×480.

- Folders on the card are the user's own (by genre, say), so the system is
  worked out from each file: its extension, then its header for ambiguous
  ones (`.bin`), and for zips either the single console ROM inside or an
  arcade ROM set.
- Files that aren't ROMs are hidden, and so are folders without ROMs. A
  folder opens as a sub-menu.
- A screenshot named after the ROM, next to it (`Sonic.md` → `Sonic.png` or
  `.jpg`), is shown in the window.
- The bar at the top shows the card's usage, a network mark while a wired
  adapter has a link, the volume, and the battery level
  and whether external power is connected.

The volume buttons work in the menu and in games (the supervisor handles
them) and the level shows in the bar. A tick sounds as the selection moves.

Controls: D-pad or left stick up/down, left/right (or L1/R1) for a page,
A to play or open, B to go back.

## How it runs

Only one program can own the display, so `r36s-menu` runs as a supervisor
that starts the menu itself (`r36s-menu -ui`), waits for it to exit with a
choice, runs `retroarch -L <core> <rom>`, and starts the menu again. The
menu position is kept in `/tmp/r36s-menu.state`.

## Building

    go test ./...
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build

Ebitengine's GBM/KMS backend isn't in a release yet, so `go.mod` pins a
commit from its main branch.

For development, `-roms DIR -any-dir` shows any folder, and
`-screenshot FILE.png` writes the menu's picture after a second and exits.

## Fonts

`assets/Caveat.ttf` and `assets/Nunito.ttf` are under the SIL Open Font
License (see the `OFL-*.txt` files next to them).
