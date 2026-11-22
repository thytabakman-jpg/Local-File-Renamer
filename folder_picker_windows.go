package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The picker runs in a child so closing the app cancels the modal dialog.
func nativeFolderPicker(start string) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole := windows.NewLazySystemDLL("ole32.dll")
	result, _, _ := ole.NewProc("CoInitializeEx").Call(0, 2)
	if int32(result) < 0 {
		return "", fmt.Errorf("Windows could not initialize the folder picker (0x%08X)", uint32(result))
	}
	defer ole.NewProc("CoUninitialize").Call()

	class := windows.GUID{Data1: 0xdc1c5a9c, Data2: 0xe88a, Data3: 0x4dde, Data4: [8]byte{0xa5, 0xa1, 0x60, 0xf8, 0x2a, 0x20, 0xae, 0xf7}}
	openDialog := windows.GUID{Data1: 0xd57c7288, Data2: 0xd4ad, Data3: 0x4768, Data4: [8]byte{0xbe, 0x02, 0x9d, 0x96, 0x95, 0x32, 0xd9, 0x60}}
	var dialog unsafe.Pointer
	result, _, _ = ole.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&class)), 0, 1, uintptr(unsafe.Pointer(&openDialog)), uintptr(unsafe.Pointer(&dialog)))
	if int32(result) < 0 {
		return "", fmt.Errorf("Windows could not open the folder picker (0x%08X)", uint32(result))
	}

	method := func(object unsafe.Pointer, slot uintptr) uintptr {
		vtable := *(*unsafe.Pointer)(object)
		return *(*uintptr)(unsafe.Add(vtable, slot*unsafe.Sizeof(uintptr(0))))
	}
	const (
		release         = 2
		show            = 3
		setOptions      = 9
		getOptions      = 10
		setFolder       = 12
		setTitle        = 17
		getResult       = 20
		getDisplayName  = 5
		pickFolders     = 0x20
		forceFileSystem = 0x40
		pathMustExist   = 0x800
		noChangeDir     = 0x8
		fileSystemPath  = 0x80058000
		cancelled       = 0x800704c7
	)
	defer syscall.SyscallN(method(dialog, release), uintptr(dialog))
	var options uint32
	result, _, _ = syscall.SyscallN(method(dialog, getOptions), uintptr(dialog), uintptr(unsafe.Pointer(&options)))
	if int32(result) < 0 {
		return "", fmt.Errorf("Windows could not read the folder picker options (0x%08X)", uint32(result))
	}
	result, _, _ = syscall.SyscallN(method(dialog, setOptions), uintptr(dialog), uintptr(options|pickFolders|forceFileSystem|pathMustExist|noChangeDir))
	if int32(result) < 0 {
		return "", fmt.Errorf("Windows could not set the folder picker options (0x%08X)", uint32(result))
	}
	title, _ := windows.UTF16PtrFromString("Choose a Local File Renamer folder")
	syscall.SyscallN(method(dialog, setTitle), uintptr(dialog), uintptr(unsafe.Pointer(title)))

	if path, err := windows.UTF16PtrFromString(start); start != "" && err == nil {
		itemInterface := windows.GUID{Data1: 0x43826d1e, Data2: 0xe718, Data3: 0x42ee, Data4: [8]byte{0xbc, 0x55, 0xa1, 0xe2, 0x61, 0xc3, 0x7b, 0xfe}}
		var folder unsafe.Pointer
		shell := windows.NewLazySystemDLL("shell32.dll")
		result, _, _ = shell.NewProc("SHCreateItemFromParsingName").Call(uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&itemInterface)), uintptr(unsafe.Pointer(&folder)))
		if int32(result) >= 0 {
			syscall.SyscallN(method(dialog, setFolder), uintptr(dialog), uintptr(folder))
			syscall.SyscallN(method(folder, release), uintptr(folder))
		}
	}
	result, _, _ = syscall.SyscallN(method(dialog, show), uintptr(dialog), 0)
	if uint32(result) == cancelled {
		return "", nil
	}
	if int32(result) < 0 {
		return "", fmt.Errorf("Windows could not show the folder picker (0x%08X)", uint32(result))
	}
	var item unsafe.Pointer
	result, _, _ = syscall.SyscallN(method(dialog, getResult), uintptr(dialog), uintptr(unsafe.Pointer(&item)))
	if int32(result) < 0 {
		return "", fmt.Errorf("Windows could not read the selected folder (0x%08X)", uint32(result))
	}
	defer syscall.SyscallN(method(item, release), uintptr(item))
	var path *uint16
	result, _, _ = syscall.SyscallN(method(item, getDisplayName), uintptr(item), fileSystemPath, uintptr(unsafe.Pointer(&path)))
	if int32(result) < 0 {
		return "", fmt.Errorf("Windows could not get the selected folder path (0x%08X)", uint32(result))
	}
	// Call after reading the string because its memory belongs to COM.
	value := windows.UTF16PtrToString(path)
	ole.NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(path)))
	return value, nil
}
