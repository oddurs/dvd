# dvd

Classic bouncing **DVD Video** screensaver for the terminal—rendered as crisp vector graphics via the **Kitty Graphics Protocol** in **Ghostty**, **Kitty**, and **WezTerm**, with automatic fallback to TrueColor half-blocks (`▀`/`▄`) or ASCII everywhere else.

Zero external dependencies. Pure Go.

## Install

```sh
go install github.com/oddurs/dvd@latest
```

Or build from source:

```sh
git clone https://github.com/oddurs/dvd.git
cd dvd
go install .
```

## Usage

```sh
dvd                  # run at 60 FPS (auto-detects Kitty graphics)
dvd -x               # screensaver mode: exit on any keypress
dvd -stats           # show bounce counter & next-corner countdown
dvd -fps 120 -black  # 120 FPS on pitch-black background
```

### Flags

```text
  -mode string   render mode: auto, kitty, ansi, or ascii (default "auto")
  -fps int       target frames per second, 15..240 (default 60)
  -speed float   movement speed multiplier (default 1.0)
  -scale float   logo width as fraction of screen width, 0.06..0.60 (default 0.21)
  -stats         show bottom status bar (bounces, corners, next corner ETA)
  -x, -screensaver
                 exit immediately on any keypress
  -black         force opaque pitch-black background instead of terminal default
  -no-video      omit 'VIDEO' label under the DVD disc
  -preview file  render a PNG preview sheet of the vector DVD logo and exit
```

### Keys

| Key | Action |
| :--- | :--- |
| `q` / `Esc` / `Ctrl+C` | Quit |
| `s` / `?` | Toggle stats HUD & next-corner ETA |
| `c` | Subtly nudge angle to line up an upcoming corner hit |
| `+` / `-` (`Up` / `Down`) | Adjust bounce speed |
| `[` / `]` | Shrink / grow logo (live vector re-rasterization) |
| `n` | Cycle palette color |
| `m` | Cycle render mode (`kitty` → `ansi` → `ascii`) |
| `Space` | Pause / resume |

## How It Works

At startup, `dvd` probes Kitty Graphics Protocol support and cell pixel dimensions (`TIOCGWINSZ`, `CSI 16t`, `CSI 14t`), rasterizes the **DVD Video** SVG paths with 8×8 supersampled scanline anti-aliasing, and uploads padded palette textures to terminal GPU memory (`a=t,f=100`). Each frame updates a constant `(c, r)` cell box using a sliding source crop (`x, y, w, h`) wrapped in synchronized output (`?2026h`), yielding tear-free single-pixel diagonal motion without cell-boundary stretching.

## License

[MIT](LICENSE)
