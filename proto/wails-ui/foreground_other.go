//go:build !windows

package main

// foreground на macOS и Linux не определяется: там смотрим IsFocused() окна тоста.
func foreground() (title string, ours bool, ok bool) { return "", false, false }
