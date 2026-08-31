//go:build linux && headless

package main

import (
	"log"

	"github.com/semaja2/trmnl-go/config"
	"github.com/semaja2/trmnl-go/display"
)

// createWindow for headless builds (-tags headless).
//
// Headless builds contain ONLY the framebuffer backend: no Fyne, no GL,
// no X11/Wayland, no CGO. The binary is pure Go and can be cross-compiled
// with a plain `go build`:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags headless -o trmnl-go-linux-arm64 .
//
// This is the intended build for headless hardware (e.g. Raspberry Pi)
// running as the console.
func createWindow(cfg *config.Config, useFyne bool, verbose bool) DisplayWindow {
	if useFyne {
		log.Fatal("-use-fyne is not available in headless builds (built with -tags headless)")
	}
	if cfg.Output != config.OutputFramebuffer {
		log.Fatal("headless builds require -output=framebuffer")
	}
	fw, err := display.NewFramebufferWindow(cfg, verbose)
	if err != nil {
		log.Fatalf("Failed to open framebuffer: %v", err)
	}
	if verbose {
		log.Println("[App] Using framebuffer display (headless build, no X11/Wayland)")
	}
	return fw
}
