//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	winUser32 = syscall.NewLazyDLL("user32.dll")
	winGdi32  = syscall.NewLazyDLL("gdi32.dll")

	procFindWindowW     = winUser32.NewProc("FindWindowW")
	procGetWindowRect   = winUser32.NewProc("GetWindowRect")
	procSetWindowRgn    = winUser32.NewProc("SetWindowRgn")
	procIsZoomed        = winUser32.NewProc("IsZoomed")
	procGetDpiForWindow = winUser32.NewProc("GetDpiForWindow")
	procCreateRoundRgn  = winGdi32.NewProc("CreateRoundRectRgn")
)

type winRect struct{ left, top, right, bottom int32 }

// roundCorners clips the window to rounded corners (radius given in logical px,
// scaled for the window's DPI) using a GDI region. While the window is maximized
// the region is removed so it fills the screen squarely. Safe to call
// repeatedly (on resize).
func roundCorners(radius int) {
	defer func() { _ = recover() }()

	title, err := syscall.UTF16PtrFromString("AutoApply")
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	// Maximized/fullscreen: drop the region so the window fills the screen.
	if z, _, _ := procIsZoomed.Call(hwnd); z != 0 {
		procSetWindowRgn.Call(hwnd, 0, 1)
		return
	}
	var r winRect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	w := int(r.right - r.left)
	h := int(r.bottom - r.top)
	if w <= 0 || h <= 0 {
		return
	}
	scale := 1.0
	if dpi, _, _ := procGetDpiForWindow.Call(hwnd); dpi >= 96 {
		scale = float64(dpi) / 96.0
	}
	d := uintptr(float64(radius*2) * scale)
	rgn, _, _ := procCreateRoundRgn.Call(0, 0, uintptr(w+1), uintptr(h+1), d, d)
	if rgn != 0 {
		procSetWindowRgn.Call(hwnd, rgn, 1) // window takes ownership of the region
	}
}
