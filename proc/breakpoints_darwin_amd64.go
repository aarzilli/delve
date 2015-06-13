package proc

import "fmt"

// TODO(darwin)
func (dbp *DebuggedProcess) setHardwareBreakpoint(reg, tid int, addr uint64) error {
	return fmt.Errorf("not implemented on darwin")
}

// TODO(darwin)
func (dbp *DebuggedProcess) clearHardwareBreakpoint(reg, tid int) error {
	return fmt.Errorf("not implemented on darwin")
}
