//go:build !windows

package main

import "io"

// cmdService is the entry point the Windows service manager uses; on
// other systems it is the same as serve.
func cmdService(args []string, stderr io.Writer, getenv func(string) string, defaultConfig string) int {
	return cmdServe(args, stderr, getenv, defaultConfig)
}
