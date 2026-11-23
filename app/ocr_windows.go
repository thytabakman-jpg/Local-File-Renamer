package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

func renderSamplePage(ctx context.Context, path string, page int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	system := os.Getenv("SystemRoot")
	if !filepath.IsAbs(system) {
		return nil, errors.New("Windows system path unavailable")
	}
	shell := filepath.Join(system, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	script := utf16.Encode([]rune(pdfPageScript))
	encoded := make([]byte, len(script)*2)
	for i, code := range script {
		binary.LittleEndian.PutUint16(encoded[i*2:], code)
	}
	image, err := os.CreateTemp("", "renamer-preview-*.jpg")
	if err != nil {
		return nil, err
	}
	defer os.Remove(image.Name())
	if err := image.Close(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded))
	cmd.Env = append(os.Environ(), "LOCAL_RENAMER_OCR_INPUT="+path, "LOCAL_RENAMER_OCR_PAGE="+strconv.Itoa(page), "LOCAL_RENAMER_OCR_OUTPUT="+image.Name())
	configureChild(cmd)
	if _, err := cmd.Output(); err != nil {
		var failed *exec.ExitError
		if errors.As(err, &failed) && len(failed.Stderr) > 0 {
			detail := strings.TrimSpace(string(failed.Stderr))
			if start := strings.Index(detail, "<S S=\"Error\">"); start >= 0 {
				detail = strings.SplitN(detail[start+13:], "</S>", 2)[0]
			}
			return nil, fmt.Errorf("Windows PDF renderer: %s", chars(detail, 360))
		}
		return nil, errors.Join(err, ctx.Err())
	}
	return os.ReadFile(image.Name())
}

const pdfPageScript = `
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Runtime.WindowsRuntime
$null = [Windows.Storage.StorageFile, Windows.Storage, ContentType=WindowsRuntime]
$null = [Windows.Data.Pdf.PdfDocument, Windows.Data.Pdf, ContentType=WindowsRuntime]
$null = [Windows.Data.Pdf.PdfPageRenderOptions, Windows.Data.Pdf, ContentType=WindowsRuntime]
$null = [Windows.Storage.Streams.InMemoryRandomAccessStream, Windows.Storage.Streams, ContentType=WindowsRuntime]
$null = [Windows.Graphics.Imaging.BitmapEncoder, Windows.Graphics.Imaging, ContentType=WindowsRuntime]
$asTask = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
    $_.Name -eq 'AsTask' -and $_.IsGenericMethod -and $_.GetParameters().Count -eq 1 -and
    $_.GetParameters()[0].ParameterType.Name -eq ('IAsyncOperation' + [char]96 + '1')
} | Select-Object -First 1
$asAction = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
    $_.Name -eq 'AsTask' -and !$_.IsGenericMethod -and $_.GetParameters().Count -eq 1 -and
    $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncAction'
} | Select-Object -First 1
function Wait-Operation($operation, $type) {
    $task = $asTask.MakeGenericMethod($type).Invoke($null, @($operation))
    return $task.GetAwaiter().GetResult()
}
$file = Wait-Operation ([Windows.Storage.StorageFile]::GetFileFromPathAsync($env:LOCAL_RENAMER_OCR_INPUT)) ([Windows.Storage.StorageFile])
$doc = Wait-Operation ([Windows.Data.Pdf.PdfDocument]::LoadFromFileAsync($file)) ([Windows.Data.Pdf.PdfDocument])
$i = [uint32]$env:LOCAL_RENAMER_OCR_PAGE
if ($i -ge $doc.PageCount) { exit 0 }
$page = $doc.GetPage($i)
$stream = [Windows.Storage.Streams.InMemoryRandomAccessStream]::new()
$copy = [System.IO.MemoryStream]::new()
try {
    $options = [Windows.Data.Pdf.PdfPageRenderOptions]::new()
    $options.BitmapEncoderId = [Windows.Graphics.Imaging.BitmapEncoder]::JpegEncoderId
    $scale = 1250 / [Math]::Max($page.Size.Width, $page.Size.Height)
    $options.DestinationWidth = [uint32][Math]::Max(1, $page.Size.Width * $scale)
    $options.DestinationHeight = [uint32][Math]::Max(1, $page.Size.Height * $scale)
    $render = $asAction.Invoke($null, @($page.RenderToStreamAsync($stream, $options)))
    $null = $render.GetAwaiter().GetResult()
    $reader = [System.IO.WindowsRuntimeStreamExtensions]::AsStreamForRead($stream.GetInputStreamAt(0))
    try {
        $reader.CopyTo($copy)
        [System.IO.File]::WriteAllBytes($env:LOCAL_RENAMER_OCR_OUTPUT, $copy.ToArray())
    } finally { $reader.Dispose() }
} finally { $copy.Dispose(); $stream.Dispose(); $page.Dispose() }
`
