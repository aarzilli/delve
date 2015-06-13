package proc

/*
#include <stddef.h>
#include <sys/types.h>
#include <sys/user.h>
#include <sys/debugreg.h>

// Exposes C macro `offsetof` which is needed for getting
// the offset of the debug register we want, and passing
// that offset to PTRACE_POKE_USER.
int offset(int reg) {
	return offsetof(struct user, u_debugreg[reg]);
}
*/
import "C"

import "fmt"

// Sets a hardware breakpoint by setting the contents of the
// debug register `reg` with the address of the instruction
// that we want to break at. There are only 4 debug registers
// DR0-DR3. Debug register 7 is the control register.
func (dbp *DebuggedProcess) setHardwareBreakpoint(reg, tid int, addr uint64) error {
	if reg < 0 || reg > 3 {
		return fmt.Errorf("invalid debug register value")
	}

	var (
		dr7off    = uintptr(C.offset(C.DR_CONTROL))
		drxoff    = uintptr(C.offset(C.int(reg)))
		drxmask   = uintptr((((1 << C.DR_CONTROL_SIZE) - 1) << uintptr(reg*C.DR_CONTROL_SIZE)) | (((1 << C.DR_ENABLE_SIZE) - 1) << uintptr(reg*C.DR_ENABLE_SIZE)))
		drxenable = uintptr(0x1) << uintptr(reg*C.DR_ENABLE_SIZE)
		drxctl    = uintptr(C.DR_RW_EXECUTE|C.DR_LEN_1) << uintptr(reg*C.DR_CONTROL_SIZE)
		dr7       uintptr
	)

	// Get current state
	var err error
	dbp.execPtraceFunc(func() { dr7, err = PtracePeekUser(tid, dr7off) })
	if err != nil {
		return err
	}

	// If addr == 0 we are expected to disable the breakpoint
	if addr == 0 {
		dr7 &= ^drxmask
		dbp.execPtraceFunc(func() { err = PtracePokeUser(tid, dr7off, dr7) })
		return err
	}

	// Set the debug register `reg` with the address of the
	// instruction we want to trigger a debug exception.
	dbp.execPtraceFunc(func() { err = PtracePokeUser(tid, drxoff, uintptr(addr)) })
	if err != nil {
		return err
	}

	// Clear dr`reg` flags
	dr7 &= ^drxmask
	// Enable dr`reg`
	dr7 |= (drxctl << C.DR_CONTROL_SHIFT) | drxenable

	// Set the debug control register. This
	// instructs the cpu to raise a debug
	// exception when hitting the address of
	// an instruction stored in dr0-dr3.
	dbp.execPtraceFunc(func() { err = PtracePokeUser(tid, dr7off, dr7) })
	return err
}

// Clears a hardware breakpoint. Essentially sets
// the debug reg to 0 and clears the control register
// flags for that reg.
func (dbp *DebuggedProcess) clearHardwareBreakpoint(reg, tid int) error {
	return dbp.setHardwareBreakpoint(reg, tid, 0)
}
