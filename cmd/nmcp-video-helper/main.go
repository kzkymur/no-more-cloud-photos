//go:build linux

package main

import (
	"os"

	"github.com/kzkymur/no-more-cloud-photos/internal/videohelper"
)

func main() {
	os.Exit(videohelper.Main(os.Args[1:], os.Stdout, os.Stderr))
}
