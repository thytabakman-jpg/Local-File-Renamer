#!/bin/sh
set -eu
cd "$(dirname "$0")"

prepare_build() {
 mkdir -p build
 lock=build/.windows-build.lock
 if ! mkdir "$lock" 2>/dev/null; then
  echo "Another Windows build is active (or its lock remains): $lock" >&2
  exit 1
 fi
 work=
 trap cleanup 0
 trap 'exit 1' HUP INT TERM
 version=$(sed -n 's/^const Current = "\([^"]*\)"$/\1/p' internal/version/version.go)
 case "$version" in
  ''|*[!0-9.]*) echo "Invalid application version" >&2; exit 1 ;;
 esac
 setup_name=LocalFileRenamer-Setup-$version.exe
 zip_name=LocalFileRenamer-Installer-$version.zip
 # A failed rebuild must not leave an earlier same-version package advertised.
 rm -f "build/$setup_name" "build/$zip_name"
 if [ "$(head -n 1 README.md)" != "# Local File Renamer $version" ]; then
  echo "README version does not match application version $version" >&2
  exit 1
 fi
 work=$(mktemp -d "$PWD/build/.windows-build.XXXXXX")
}

cleanup() {
 if [ -n "$work" ]; then rm -rf "$work"; fi
 rm -f installer/payload.zip app/icon_windows_amd64.syso installer/icon_windows_amd64.syso
 rmdir "$lock"
}

generate_icons() {
 if [ -n "${RSRC_BIN:-}" ]; then
  "$RSRC_BIN" -ico app/icon.ico -arch amd64 -o app/icon_windows_amd64.syso
 else
  go run github.com/akavel/rsrc@v0.10.2 -ico app/icon.ico -arch amd64 -o app/icon_windows_amd64.syso
 fi
 cp app/icon_windows_amd64.syso installer/icon_windows_amd64.syso
}

compile_application() {
 export GOOS=windows GOARCH=amd64 CGO_ENABLED=0
 go build -buildvcs=false -trimpath -ldflags '-s -w -H windowsgui' -o "$work/LocalFileRenamer.exe" ./app
 go build -buildvcs=false -trimpath -ldflags '-s -w' -o "$work/LocalFileRenamer-MCP.exe" ./launcher
}

obtain_engine() {
 archive=${LLAMA_CPU_ZIP:-$work/llama-win.zip}
 if [ -z "${LLAMA_CPU_ZIP:-}" ]; then
  curl --fail --silent --show-error --location \
   https://github.com/ggml-org/llama.cpp/releases/download/b12588/llama-b12588-bin-win-cpu-x64.zip \
   --output "$archive"
 fi
}

assemble_payload() {
 python3 - "$archive" "$work/LocalFileRenamer.exe" "$work/LocalFileRenamer-MCP.exe" <<'PY'
import hashlib
import sys
import zipfile
from pathlib import Path

archive, app, launcher = map(Path, sys.argv[1:])
expected = "e61b81d5e11f49edc88c00aa7513291e0be6a341ff8498083f775ee721568be4"
if hashlib.sha256(archive.read_bytes()).hexdigest() != expected:
    raise SystemExit("The llama.cpp binary archive did not match its expected checksum.")
engine = {"llama-server.exe", "llama-server-impl.dll", "llama-common.dll", "llama.dll",
          "mtmd.dll", "ggml.dll", "ggml-base.dll", "libomp.dll"}
with zipfile.ZipFile(archive) as upstream, zipfile.ZipFile("installer/payload.zip", "w", zipfile.ZIP_DEFLATED) as setup:
    names = set(upstream.namelist())
    missing = engine - names
    if missing:
        raise SystemExit(f"Missing engine files: {', '.join(sorted(missing))}")
    for name in sorted(names):
        if name in engine or name.startswith("ggml-cpu-") and name.endswith(".dll"):
            setup.writestr(f"engine/{name}", upstream.read(name))
    setup.write(app, app.name)
    setup.write(launcher, launcher.name)
    setup.write("LICENSE")
    setup.write("README.md")
    for path in sorted(Path("licenses").iterdir()):
        setup.write(path, path.as_posix())
with zipfile.ZipFile("installer/payload.zip") as setup:
    if setup.testzip() is not None:
        raise SystemExit("Installer payload failed ZIP verification.")
PY
}

compile_installer() {
 go build -buildvcs=false -trimpath -ldflags '-s -w -H windowsgui' -o "$work/$setup_name" ./installer
}

package_distribution() {
 python3 - "$work/$setup_name" "$work/$zip_name" <<'PYZIP'
import sys
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile

setup, output = map(Path, sys.argv[1:])
if setup.stat().st_size == 0:
    raise SystemExit("Installer executable is empty.")
with ZipFile(output, "w", ZIP_DEFLATED) as archive:
    archive.write(setup, setup.name)
with ZipFile(output) as archive:
    if archive.namelist() != [setup.name] or archive.testzip() is not None:
        raise SystemExit("Installer ZIP verification failed.")
PYZIP
 mv "$work/$setup_name" "build/$setup_name"
 if ! mv "$work/$zip_name" "build/$zip_name"; then
  rm -f "build/$setup_name"
  exit 1
 fi
}

prepare_build
generate_icons
compile_application
obtain_engine
assemble_payload
compile_installer
package_distribution
