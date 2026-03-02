package session

import (
	"io"
	"os"
	"os/exec"
)

// startResult is the platform-common return value from process startup.
type startResult struct {
	pty         io.ReadWriteCloser
	cmd         *exec.Cmd
	rawPipe     *os.File // Unix: FIFO reader, Windows: nil
	rawPipePath string   // Unix: FIFO path, Windows: ""
	tmuxName    string   // Unix: tmux session name, Windows: ""
}
