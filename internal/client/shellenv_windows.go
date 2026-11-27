//go:build windows

package client

// LoginShellPath — на Windows PATH процесса уже полный.
func LoginShellPath() string { return "" }
