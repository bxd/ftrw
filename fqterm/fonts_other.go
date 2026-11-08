//go:build !windows

package main

// listSystemFonts is a no-op on non-Windows platforms; the font picker simply
// shows the default entry. Font enumeration here is Windows-only by design.
func listSystemFonts() []string {
	return nil
}
