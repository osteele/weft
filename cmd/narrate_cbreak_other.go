//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package cmd

import "errors"

type narrateCbreakState struct{}

func makeCbreak(uintptr) (*narrateCbreakState, error) {
	return nil, errors.New("cbreak terminal mode is not supported on this platform")
}

func restoreCbreak(uintptr, *narrateCbreakState) {}
