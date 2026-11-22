package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func linked(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	stat, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && stat.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// Win32 calls below accept extended-length absolute paths without a registry change.
func winPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}
func identity(path string, _ os.FileInfo) (string, error) {
	p, err := windows.UTF16PtrFromString(winPath(path))
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var f windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x", f.VolumeSerialNumber, f.FileIndexHigh, f.FileIndexLow), nil
}
func renameNoReplace(from, to string) error {
	a, err := windows.UTF16PtrFromString(winPath(from))
	if err != nil {
		return err
	}
	b, err := windows.UTF16PtrFromString(winPath(to))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(a, b, windows.MOVEFILE_WRITE_THROUGH)
}

func recycleFile(path string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole := windows.NewLazySystemDLL("ole32.dll")
	shell := windows.NewLazySystemDLL("shell32.dll")
	check := func(result uintptr, operation string) error {
		if int32(result) < 0 {
			return fmt.Errorf("Windows could not %s (0x%08X)", operation, uint32(result))
		}
		return nil
	}
	result, _, _ := ole.NewProc("CoInitializeEx").Call(0, 2) // STA
	if err := check(result, "initialize the Recycle Bin operation"); err != nil {
		return err
	}
	defer ole.NewProc("CoUninitialize").Call()

	fileOperation := windows.GUID{Data1: 0x3ad05575, Data2: 0x8857, Data3: 0x4850, Data4: [8]byte{0x92, 0x77, 0x11, 0xb8, 0x5b, 0xdb, 0x8e, 0x09}}
	fileOperationInterface := windows.GUID{Data1: 0x947aab5f, Data2: 0x0a5c, Data3: 0x4c13, Data4: [8]byte{0xb4, 0xd6, 0x4b, 0xf7, 0x83, 0x6f, 0xc9, 0xf8}}
	shellItemInterface := windows.GUID{Data1: 0x43826d1e, Data2: 0xe718, Data3: 0x42ee, Data4: [8]byte{0xbc, 0x55, 0xa1, 0xe2, 0x61, 0xc3, 0x7b, 0xfe}}
	var operation, item unsafe.Pointer
	result, _, _ = ole.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&fileOperation)), 0, 1,
		uintptr(unsafe.Pointer(&fileOperationInterface)), uintptr(unsafe.Pointer(&operation)))
	if err := check(result, "open the Recycle Bin operation"); err != nil {
		return err
	}
	method := func(object unsafe.Pointer, slot uintptr) uintptr {
		vtable := *(*unsafe.Pointer)(object)
		return *(*uintptr)(unsafe.Add(vtable, slot*unsafe.Sizeof(uintptr(0))))
	}
	const (
		release = 2
		setOperationFlags = 5
		deleteItem = 18
		performOperations = 21
		getAnyOperationsAborted = 22
		recycleOnly = 0x00080000 // FOFX_RECYCLEONDELETE
		noPrompts = 0x0004 | 0x0010 | 0x0400 // FOF_SILENT | FOF_NOCONFIRMATION | FOF_NOERRORUI
	)
	defer syscall.SyscallN(method(operation, release), uintptr(operation))
	filename, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	result, _, _ = shell.NewProc("SHCreateItemFromParsingName").Call(uintptr(unsafe.Pointer(filename)), 0,
		uintptr(unsafe.Pointer(&shellItemInterface)), uintptr(unsafe.Pointer(&item)))
	if err := check(result, "open the selected file"); err != nil {
		return err
	}
	defer syscall.SyscallN(method(item, release), uintptr(item))
	result, _, _ = syscall.SyscallN(method(operation, setOperationFlags), uintptr(operation), recycleOnly|noPrompts)
	if err := check(result, "set the Recycle Bin options"); err != nil {
		return err
	}
	result, _, _ = syscall.SyscallN(method(operation, deleteItem), uintptr(operation), uintptr(item), 0)
	if err := check(result, "queue the selected file for recycling"); err != nil {
		return err
	}
	result, _, _ = syscall.SyscallN(method(operation, performOperations), uintptr(operation))
	if err := check(result, "move the file to the Recycle Bin"); err != nil {
		return err
	}
	var aborted int32
	result, _, _ = syscall.SyscallN(method(operation, getAnyOperationsAborted), uintptr(operation), uintptr(unsafe.Pointer(&aborted)))
	if err := check(result, "verify the Recycle Bin operation"); err != nil {
		return err
	}
	if aborted != 0 {
		return fmt.Errorf("The file was not moved to the Recycle Bin")
	}
	return nil
}
func configureChild(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | 0x00004000}
}

func replaceExisting(from, to string) error {
	a, err := windows.UTF16PtrFromString(winPath(from))
	if err != nil {
		return err
	}
	b, err := windows.UTF16PtrFromString(winPath(to))
	if err != nil {
		return err
	}
	replace := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")
	ok, _, err := replace.Call(uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(a)), 0, 0, 0, 0)
	if ok == 0 {
		return err
	}
	return nil
}

func configureModelPriority(c *exec.Cmd, priority string) {
	configureChild(c)
	switch priority {
	case "normal":
		c.SysProcAttr.CreationFlags = 0x08000000
	case "idle":
		c.SysProcAttr.CreationFlags = 0x08000000 | 0x00000040
	}
}
func applyModelPriority(_ *exec.Cmd, _ string) {}
