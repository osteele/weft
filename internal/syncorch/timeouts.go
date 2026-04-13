package syncorch

import "time"

const (
	FastSSHTimeout    = 2 * time.Second
	DefaultSSHTimeout = 5 * time.Second
	NormalSSHTimeout  = 30 * time.Second

	FastHostTimeout   = 30 * time.Second
	NormalHostTimeout = 10 * time.Minute

	FastCloudTimeoutCLI   = 10 * time.Second
	NormalCloudTimeoutCLI = 30 * time.Second

	FastCloudTimeoutTUI   = 10 * time.Second
	NormalCloudTimeoutTUI = 60 * time.Second

	SyncInterval = 60 * time.Second
)
