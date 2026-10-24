//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	shell32              = syscall.NewLazyDLL("shell32.dll")
	procSHFileOperationW = shell32.NewProc("SHFileOperationW")
)

const (
	foDelete          = 3
	fofSilent         = 0x0004 // no progress dialog
	fofNoConfirmation = 0x0010 // answer "yes" to any prompt
	fofAllowUndo      = 0x0040 // send to the Recycle Bin instead of erasing
	fofNoErrorUI      = 0x0400 // no error dialog
)

// shFileOpStructW mirrors the Win32 SHFILEOPSTRUCTW structure.
type shFileOpStructW struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

// sendToTrash sends path to the Windows Recycle Bin via SHFileOperationW
// with FOF_ALLOWUNDO, so items can be restored from the Recycle Bin. It
// never falls back to deleting.
func sendToTrash(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	// pFrom is a double NUL-terminated list of paths; a single path
	// therefore needs one extra NUL beyond Go's own terminator.
	from, err := syscall.UTF16FromString(abs + "\x00")
	if err != nil {
		return err
	}
	op := shFileOpStructW{
		wFunc:  foDelete,
		pFrom:  &from[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI,
	}
	if r1, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op))); r1 != 0 {
		return fmt.Errorf("cannot move %q to the Recycle Bin (SHFileOperationW code %d)", abs, r1)
	}
	if op.fAnyOperationsAborted != 0 {
		return fmt.Errorf("moving %q to the Recycle Bin was aborted", abs)
	}
	return nil
}

// ensureTrashAvailable reports that the Recycle Bin is usable. The
// Recycle Bin always exists on Windows (it is created on first use), so
// this only verifies that SHFileOperationW can be found in shell32.dll.
func ensureTrashAvailable() (string, error) {
	if err := procSHFileOperationW.Find(); err != nil {
		return "", fmt.Errorf("SHFileOperationW not available: %w", err)
	}
	return "Recycle Bin", nil
}
