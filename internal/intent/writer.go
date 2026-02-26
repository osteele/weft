package intent

import (
	"fmt"

	"github.com/osteele/weft/internal/ssh"
)

const (
	// RemoteIntentDir is where intent files are stored on the coordinator host.
	RemoteIntentDir = "~/.cache/weft/intents"
)

// WriteIntent writes an intent file to the coordinator host via SSH.
// It uses atomic write (write to temp, then mv) to prevent partial reads.
func WriteIntent(host string, intent *Intent) error {
	data, err := intent.Marshal()
	if err != nil {
		return fmt.Errorf("marshal intent: %w", err)
	}

	filename := intent.Filename()
	remotePath := RemoteIntentDir + "/" + filename

	// Atomic write: write to temp file, then move into place.
	// This prevents the coordinator's fsnotify watcher from reading a partial file.
	cmd := fmt.Sprintf(
		"mkdir -p %s && f=$(mktemp %s/.tmp.XXXXXX) && cat > \"$f\" && mv \"$f\" %s",
		RemoteIntentDir, RemoteIntentDir, remotePath)

	_, stderr, err := ssh.RunWithStdin(host, cmd, string(data))
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return fmt.Errorf("coordinator %s unreachable: %w", host, err)
		}
		return fmt.Errorf("write intent to %s: %w", host, err)
	}
	return nil
}
