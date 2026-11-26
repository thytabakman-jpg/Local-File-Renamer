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
 publishing=0
 trap cleanup 0
 trap 'exit 1' HUP INT TERM
 version=$(sed -n 's/^const Current = "\([^"]*\)"$/\1/p' internal/version/version.go)
 case "$version" in
  ''|*[!0-9.]*) echo "Invalid application version" >&2; exit 1 ;;
 esac
 setup_name=LocalFileRenamer-Setup-$version.exe
 zip_name=LocalFileRenamer-Installer-$version.zip
 if [ "$(head -n 1 README.md)" != "# Local File Renamer $version" ]; then
  echo "README version does not match application version $version" >&2
  exit 1
 fi
 work=$(mktemp -d "$PWD/build/.windows-build.XXXXXX")
}

cleanup() {
 result=$?
 rollback_failed=0
 trap - 0
 set +e
 if [ "$result" -ne 0 ]; then
  if [ "$publishing" -eq 1 ]; then
   for name in "$setup_name" "$zip_name"; do
    if [ -f "$work/previous/$name" ]; then
     mv -f "$work/previous/$name" "build/$name" || rollback_failed=1
    else
     rm -f "build/$name" || rollback_failed=1
    fi
   done
  fi
  if [ "$rollback_failed" -eq 1 ]; then
   result=2
   echo "Build failed; output rollback may be incomplete. Recovery files: $work" >&2
   work=
  else
   echo "Build failed; existing distribution files were not replaced." >&2
  fi
 fi
 if [ -n "$work" ]; then rm -rf "$work" || result=1; fi
 rm -f installer/payload.zip app/icon_windows_amd64.syso installer/icon_windows_amd64.syso || result=1
 rmdir "$lock" || result=1
 exit "$result"
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
import struct

with setup.open("rb") as binary:
    header = binary.read(64)
    if len(header) != 64 or header[:2] != b"MZ":
        raise SystemExit("Installer is not a Windows executable.")
    binary.seek(struct.unpack_from("<I", header, 60)[0])
    pe = binary.read(26)
    if len(pe) != 26 or pe[:4] != b"PE\x00\x00" or struct.unpack_from("<H", pe, 4)[0] != 0x8664 or struct.unpack_from("<H", pe, 24)[0] != 0x20b:
        raise SystemExit("Installer is not a Windows x64 executable.")
with ZipFile(output, "w", ZIP_DEFLATED) as archive:
    archive.write(setup, setup.name)
with ZipFile(output) as archive:
    if archive.namelist() != [setup.name] or archive.testzip() is not None:
        raise SystemExit("Installer ZIP verification failed.")
PYZIP
 mkdir "$work/previous"
 for name in "$setup_name" "$zip_name"; do
  if [ -f "build/$name" ]; then cp -p "build/$name" "$work/previous/$name"; fi
 done
 publishing=1
 mv -f "$work/$setup_name" "build/$setup_name"
 mv -f "$work/$zip_name" "build/$zip_name"
 publishing=0
 echo "Windows build verified and published: build/$zip_name"
}

prepare_build
generate_icons
compile_application
obtain_engine
assemble_payload
compile_installer
package_distribution
