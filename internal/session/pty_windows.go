//go:build windows

package session

import (
	"fmt"
	"os"
)

// Resize resizes the ConPTY terminal on Windows.
func (s *Session) Resize(cols, rows uint16) error {
	s.mu.Lock()
	ptmx := s.PTY
	prevCols := s.lastCols
	prevRows := s.lastRows
	s.mu.Unlock()

	if ptmx == nil {
		return os.ErrClosed
	}

	// Skip if dimensions haven't changed
	if cols == prevCols && rows == prevRows {
		return nil
	}

	// ConPTY resize: type-assert to *conPTY to access the underlying resize
	cpty, ok := ptmx.(*conPTY)
	if !ok {
		return fmt.Errorf("PTY is not a ConPTY instance")
	}
	if err := cpty.cpty.Resize(int(cols), int(rows)); err != nil {
		return err
	}

	s.mu.Lock()
	s.lastCols = cols
	s.lastRows = rows
	s.mu.Unlock()

	return nil
}
