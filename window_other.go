//go:build !windows

package main

// roundCorners is a no-op on non-Windows platforms.
func roundCorners(radius int) {}
