//go:build windows

package main

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"localrenamer/internal/mcplaunch"
)

//go:embed payload.zip
var payload []byte

const appName = "Local File Renamer"

func main() {
	if dialog("Install Local File Renamer "+installVersion+" for your Windows account?", 0x41) != 1 {
		return
	}
	app, warn, err := install()
	if err != nil {
		dialog("Installation could not finish.\n\n"+err.Error(), 0x10)
		return
	}
	if warn != nil {
		dialog("The app is installed.\n\n"+warn.Error(), 0x30)
	}
	if oldVersionRunning() {
		dialog("Version "+installVersion+" is installed. To open it, close the earlier Renamer using Close app in its browser window, then choose Local File Renamer from the Start menu.\n\nNew executable: "+app, 0x40)
		return
	}
	if dialog("Local File Renamer is installed. Open it now?", 0x44) == 6 {
		if err = child(app).Start(); err != nil {
			dialog("Open Local File Renamer from the Start menu.\n\n"+err.Error(), 0x30)
		}
	}
}

func dialog(message string, kind uintptr) uintptr {
	text, _ := windows.UTF16PtrFromString(message)
	title, _ := windows.UTF16PtrFromString(appName + " Setup")
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW")
	answer, _, _ := syscall.SyscallN(proc.Addr(), 0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), kind)
	return answer
}

func install() (string, error, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return "", nil, errors.New("Windows local app data folder was not found")
	}
	programs := filepath.Join(local, "Programs")
	app, err := installFiles(programs, unpack)
	if err != nil {
		return "", nil, err
	}
	warn := ensureCRT(filepath.Dir(app))
	if err := mcplaunch.Install(programs, app, filepath.Join(filepath.Dir(app), mcplaunch.Executable)); err != nil {
		warn = errors.Join(warn, fmt.Errorf("Stable MCP launcher could not be installed: %w", err))
	}
	if err := shortcut(app); err != nil {
		warn = errors.Join(warn, fmt.Errorf("Start menu shortcut could not be updated. Open the new app at %s", app))
	}
	return app, warn, nil
}

func oldVersionRunning() bool {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	probe, err := client.Get("http://127.0.0.1:39173/status")
	if err != nil {
		return false
	}
	defer probe.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(probe.Body, 128))
	version := strings.TrimSpace(string(b))
	return strings.HasPrefix(version, appName) && version != appName+" v"+installVersion
}

func unpack(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	z, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return err
	}
	for _, f := range z.File {
		if !filepath.IsLocal(filepath.FromSlash(f.Name)) || f.FileInfo().IsDir() || !f.Mode().IsRegular() {
			return fmt.Errorf("unexpected installer entry: %s", f.Name)
		}
		path := filepath.Join(dir, filepath.FromSlash(f.Name))
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err = extract(f, path); err != nil {
			return err
		}
	}
	return nil
}
func extract(f *zip.File, path string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dest, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dest, src)
	return errors.Join(copyErr, dest.Close())
}
func child(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return c
}
func shortcut(exe string) error {
	c := child("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `
$ErrorActionPreference = 'Stop'
$shell = New-Object -ComObject WScript.Shell
$link = $shell.CreateShortcut((Join-Path ([Environment]::GetFolderPath('Programs')) 'Local File Renamer.lnk'))
$link.TargetPath = $env:RENAMER_PROGRAM
$link.WorkingDirectory = Split-Path -LiteralPath $env:RENAMER_PROGRAM
$link.IconLocation = $env:RENAMER_PROGRAM + ',0'
$link.Save()
`)
	c.Env = append(os.Environ(), "RENAMER_PROGRAM="+exe)
	return c.Run()
}
func ensureCRT(dir string) error {
	exe := filepath.Join(dir, "engine", "llama-server.exe")
	c := child(exe, "--version")
	c.Dir = filepath.Dir(exe)
	if c.Run() == nil {
		return nil
	}
	if dialog("The local AI engine could not start. Install Microsoft's Visual C++ runtime now? Windows may ask for administrator permission.", 0x34) != 6 {
		return errors.New("You can still choose Run without local AI. Microsoft’s Visual C++ x64 runtime is required for local AI on this PC.")
	}
	c = child("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `
$ErrorActionPreference = 'Stop'
$p = Join-Path ([IO.Path]::GetTempPath()) ('vc_redist.' + [Guid]::NewGuid().ToString('N') + '.x64.exe')
try {
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
  Invoke-WebRequest -UseBasicParsing 'https://aka.ms/vc14/vc_redist.x64.exe' -OutFile $p
  $signature = Get-AuthenticodeSignature -LiteralPath $p
  if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch '(?:^|,\s*)O=Microsoft Corporation(?:,|$)') {
    throw 'Microsoft signature verification failed.'
  }
  $installer = Start-Process -FilePath $p -ArgumentList '/install','/passive','/norestart' -Verb RunAs -Wait -PassThru
  if ($installer.ExitCode -notin @(0,1638,3010)) { throw ('Windows runtime installation returned ' + $installer.ExitCode) }
} finally { Remove-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue }
`)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("Microsoft runtime setup did not complete: %s", strings.TrimSpace(string(out)))
	}
	c = child(exe, "--version")
	c.Dir = filepath.Dir(exe)
	if c.Run() != nil {
		return errors.New("Restart Windows and try Local AI. Run without local AI is also available.")
	}
	return nil
}
