//go:build !darwin && !linux

package main

import (
	"github.com/semaja2/trmnl-go/config"
	"github.com/semaja2/trmnl-go/display"
)

// createWindow creates the appropriate window for the platform
func createWindow(cfg *config.Config, useFyne bool, verbose bool) DisplayWindow {
	// On Windows, always use Fyne (the framebuffer backend is Linux-only)
	return display.NewWindow(cfg, verbose)
}
