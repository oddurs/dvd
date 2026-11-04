// dvd is a zero-dependency terminal DVD Video screensaver written in Go.
//
// In terminals supporting the Kitty Graphics Protocol (Ghostty, Kitty, WezTerm,
// including inside tmux when allow-passthrough is enabled), dvd rasterizes the
// classic DVD Video vector paths at 2x sub-pixel density with an 8x8
// supersampled active-edge scanline rasterizer, encodes a single indexed PNG
// with in-place PLTE+CRC32 re-tinting to cache padded palette textures in GPU
// memory, and animates at up to 360 FPS (240 FPS default) with half-pixel GPU
// bilinear precision and zero heap allocations on frame, HUD, and input paths.
//
// Elsewhere, dvd falls back automatically to phase-cached 4x4 sub-cell
// anti-aliased TrueColor Unicode half-block rendering (▀/▄/█) with cross-row
// SGR deduplication or monochrome/colored ASCII art.
//
// Usage:
//
//	dvd [flags]
//
// Install:
//
//	go install github.com/oddurs/dvd@latest
package main
