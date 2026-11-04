# dvd

<p align="center">
  <img src="assets/demo.svg" alt="dvd terminal screensaver preview" width="720">
</p>

Classic bouncing **DVD Video** screensaver for the terminal—rendered as crisp vector graphics via the **Kitty Graphics Protocol** in **Ghostty**, **Kitty**, and **WezTerm** (including inside `tmux` with `allow-passthrough on`), with automatic fallback to sub-row TrueColor half-blocks (`▀`/`▄`) or ASCII everywhere else.

Zero external dependencies. Pure Go. Runs on macOS, Linux, BSD, and Windows.

## Install

```sh
go install github.com/oddurs/dvd@latest
```

Or grab a prebuilt archive from [Releases](https://github.com/oddurs/dvd/releases), or build from source:

```sh
git clone https://github.com/oddurs/dvd.git
cd dvd
go install .
```

## Usage

```sh
dvd                  # run at 60 FPS (auto-detects Kitty graphics)
dvd -x               # screensaver mode: exit on any key or mouse movement
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
                 exit immediately on any keypress or mouse activity
  -black         force opaque pitch-black background instead of terminal default
  -no-video      omit 'VIDEO' label under the DVD disc
  -preview file  render a PNG preview sheet of the vector DVD logo and exit
  -v, -version   print version and exit
```

Respects [`NO_COLOR`](https://no-color.org/) and `tmux` DCS passthrough (`set -g allow-passthrough on`) automatically.

### Keys

| Key | Action |
| :--- | :--- |
| `q` / `Esc` / `Ctrl+C` | Quit |
| `s` / `?` | Toggle stats HUD & next-corner ETA |
| `c` | Subtly nudge angle to line up an upcoming corner hit |
| `r` | Randomize position, angle, and palette color |
| `+` / `-` (`Up` / `Down`) | Adjust bounce speed |
| `[` / `]` | Shrink / grow logo (live vector re-rasterization) |
| `n` | Cycle palette color |
| `m` | Cycle render mode (`kitty` → `ansi` → `ascii`) |
| `Ctrl+L` | Force full screen refresh |
| `Ctrl+Z` | Suspend to shell (resume with `fg`) |
| `Space` | Pause / resume |

## How It Works

At startup, `dvd` probes Kitty Graphics Protocol support and cell pixel dimensions (`TIOCGWINSZ` / Win32 console metrics, `CSI 16t`, `CSI 14t`), rasterizes the **DVD Video** SVG paths with 8×8 supersampled scanline anti-aliasing, and uploads padded palette textures to terminal GPU memory (`a=t,f=100`). Each frame updates a constant `(c, r)` cell box using a sliding source crop (`x, y, w, h`) wrapped in synchronized output (`?2026h`) with zero heap allocations per frame, yielding tear-free single-pixel diagonal motion without cell-boundary stretching.

## License

[MIT](LICENSE)
