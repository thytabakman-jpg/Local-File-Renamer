package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func linked(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }
func identity(_ string, info os.FileInfo) (string, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot read file identity")
	}
	return fmt.Sprintf("%x:%x", st.Dev, st.Ino), nil
}
func renameNoReplace(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}

const recycleAvailable = false

func recycleFile(_ string) error { return fmt.Errorf("The Windows Recycle Bin is unavailable") }
func configureChild(c *exec.Cmd) {}

func replaceExisting(from, to string) error { return os.Rename(from, to) }

func configureModelPriority(c *exec.Cmd, _ string) { configureChild(c) }
func applyModelPriority(c *exec.Cmd, priority string) {
	nice := 0
	switch priority {
	case "below_normal":
		nice = 7
	case "idle":
		nice = 19
	}
	if nice > 0 {
		_ = unix.Setpriority(unix.PRIO_PROCESS, c.Process.Pid, nice)
	}
}

func nativeFolderPicker(string) (string, error) {
	return "", errors.New("The native Windows folder picker is available on Windows")
}

func testProcessMemory(pid int) (resident, peak uint64) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "VmRSS:":
			resident, _ = strconv.ParseUint(fields[1], 10, 64)
		case "VmHWM:":
			peak, _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return resident * 1024, peak * 1024
}
