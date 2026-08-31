//go:build linux && !headless

package main

import (
	"log"

	"github.com/semaja2/trmnl-go/config"
	"github.com/semaja2/trmnl-go/display"
)

// createWindow creates the appropriate window for Linux.
// Supports the framebuffer backend (-output=framebuffer), which bypasses
// X11/Wayland entirely.
func createWindow(cfg *config.Config, useFyne bool, verbose bool) DisplayWindow {
	if !useFyne && cfg.Output == config.OutputFramebuffer {
		fw, err := display.NewFramebufferWindow(cfg, verbose)
		if err != nil {
			log.Printf("Warning: framebuffer backend unavailable (%v), falling back to Fyne window", err)
		} else {
			if verbose {
				log.Println("[App] Using framebuffer display (no X11/Wayland)")
			}
			return fw
		}
	}
	return display.NewWindow(cfg, verbose)
}
