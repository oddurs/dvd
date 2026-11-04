// dvd is a zero-dependency terminal DVD Video screensaver written in Go.
//
// In terminals supporting the Kitty Graphics Protocol (Ghostty, Kitty, WezTerm,
// including inside tmux when allow-passthrough is enabled), dvd rasterizes the
// classic DVD Video vector paths at 2x sub-pixel density with an O(1)-span
// 8x8 supersampled active-edge rasterizer, encodes a single indexed PNG via a
// zero-alloc flate+adler32 stream with partial Base64 PLTE+CRC32 patching, and
// animates at up to 360 FPS (240 FPS default) with a drift-compensated frame
// pacer and zero heap allocations across upload, frame, HUD, and input paths.
//
// Elsewhere, dvd falls back automatically to phase-cached 4x4 sub-cell
// anti-aliased TrueColor Unicode half-block rendering (▀/▄/█) with pre-baked
// SGR lookup tables and cross-row SGR deduplication, or ASCII art.
//
// Usage:
//
//	dvd [flags]
//
// Install:
//
//	go install github.com/oddurs/dvd@latest
package main
