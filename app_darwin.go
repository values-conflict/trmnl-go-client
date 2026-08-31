//go:build darwin

package main

import (
	"github.com/semaja2/trmnl-go/config"
	"github.com/semaja2/trmnl-go/display"
)

// createWindow creates the appropriate window for the platform
func createWindow(cfg *config.Config, useFyne bool, verbose bool) DisplayWindow {
	if !useFyne && cfg.Output == config.OutputFramebuffer {
		// The framebuffer backend is Linux-only; fall back to the native window.
		if verbose {
			println("[App] Note: framebuffer output is Linux-only, using native macOS window instead")
		}
	}
	if !useFyne {
		if verbose {
			println("[App] Using native macOS window")
		}
		return display.NewNativeWindow(cfg, verbose)
	}
	if verbose {
		println("[App] Using Fyne window (forced via -use-fyne flag)")
	}
	return display.NewWindow(cfg, verbose)
}
