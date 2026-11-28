//go:build !windows

package main

// foreground на macOS и Linux не определяется: там selftest-ui смотрит IsFocused() панели.
func foreground() (title string, ours bool, ok bool) { return "", false, false }
