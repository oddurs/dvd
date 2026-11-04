// dvd is a zero-dependency terminal DVD Video screensaver written in Go.
//
// In terminals supporting the Kitty Graphics Protocol (Ghostty, Kitty, WezTerm,
// including inside tmux when allow-passthrough is enabled), dvd rasterizes the
// classic DVD Video vector paths with 8x8 supersampled anti-aliasing, caches
// padded palette textures in GPU memory, and animates at 60-240 FPS with
// single-pixel precision using constant (c, r) cell boxes and sliding source
// crops wrapped in synchronized output (?2026h).
//
// Elsewhere, dvd falls back automatically to sub-row TrueColor Unicode
// half-block rendering (▀/▄) or monochrome/colored ASCII art.
//
// Usage:
//
//	dvd [flags]
//
// Install:
//
//	go install github.com/oddurs/dvd@latest
package main
