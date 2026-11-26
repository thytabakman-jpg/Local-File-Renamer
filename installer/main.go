//go:build windows

package main

import (
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
	if renamerRunning() {
		dialog("Version "+installVersion+" is installed. To open it, close the running Renamer using Close app in its browser window, then choose Local File Renamer from the Start menu.\n\nNew executable: "+app, 0x40)
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

func renamerRunning() bool {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	probe, err := client.Get("http://127.0.0.1:39173/status")
	if err != nil {
		return false
	}
	defer probe.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(probe.Body, 128))
	version := strings.TrimSpace(string(b))
	return probe.StatusCode == http.StatusOK && strings.HasPrefix(version, appName+" v")
}

func unpack(dir string) error {
	return unpackArchive(dir, payload)
}

func child(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	hideWindow(c)
	return c
}

func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
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
	probe := probeEngine(exe, 15*time.Second, hideWindow)
	if probe.err == nil {
		return nil
	}
	missing := missingMicrosoftRuntime(filepath.Dir(exe), probe)
	if len(missing) == 0 {
		return fmt.Errorf("local AI engine could not start; use Run without local AI if needed. %s", probe.diagnostic())
	}
	if dialog("The engine reports a missing DLL. Imported Microsoft runtime files not found in standard locations: "+strings.Join(missing, ", ")+". Install Microsoft's Visual C++ runtime now? Windows may ask for administrator permission.", 0x34) != 6 {
		return errors.New("Microsoft runtime installation declined. Run without local AI is available.")
	}
	if err := installCRT(); err != nil {
		return err
	}
	if retry := probeEngine(exe, 15*time.Second, hideWindow); retry.err != nil {
		return fmt.Errorf("local AI engine still could not start after runtime setup: %s. Restart Windows or use Run without local AI.", retry.diagnostic())
	}
	return nil
}

func installCRT() error {
	c := child("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `
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
		return fmt.Errorf("Microsoft runtime setup did not complete: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
