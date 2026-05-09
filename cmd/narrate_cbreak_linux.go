//go:build linux

package cmd

import "golang.org/x/sys/unix"

type narrateCbreakState = unix.Termios

func makeCbreak(fd uintptr) (*narrateCbreakState, error) {
	oldState, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	if err != nil {
		return nil, err
	}
	newState := *oldState
	newState.Lflag &^= unix.ICANON | unix.ECHO
	newState.Cc[unix.VMIN] = 1
	newState.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(int(fd), unix.TCSETS, &newState); err != nil {
		return nil, err
	}
	return oldState, nil
}

func restoreCbreak(fd uintptr, state *narrateCbreakState) {
	if state != nil {
		_ = unix.IoctlSetTermios(int(fd), unix.TCSETS, state)
	}
}
