#!/bin/sh
set -eu
cd "$(dirname "$0")"
work=$(mktemp -d)
trap 'rm -rf "$work"; rm -f installer/payload.zip app/icon_windows_amd64.syso installer/icon_windows_amd64.syso' EXIT
mkdir -p build
if [ -n "${RSRC_BIN:-}" ]; then
 "$RSRC_BIN" -ico app/icon.ico -arch amd64 -o app/icon_windows_amd64.syso
else
 go run github.com/akavel/rsrc@v0.10.2 -ico app/icon.ico -arch amd64 -o app/icon_windows_amd64.syso
fi
cp app/icon_windows_amd64.syso installer/icon_windows_amd64.syso
export GOOS=windows GOARCH=amd64 CGO_ENABLED=0
go build -buildvcs=false -trimpath -ldflags '-s -w -H windowsgui' -o "$work/LocalFileRenamer.exe" ./app
archive=${LLAMA_CPU_ZIP:-$work/llama-win.zip}
if [ -z "${LLAMA_CPU_ZIP:-}" ]; then
 curl --fail --silent --show-error --location \
  https://github.com/ggml-org/llama.cpp/releases/download/b12588/llama-b12588-bin-win-cpu-x64.zip \
  --output "$archive"
fi
python3 - "$archive" "$work/LocalFileRenamer.exe" <<'PY'
import hashlib
import sys
import zipfile
from pathlib import Path

archive, app = map(Path, sys.argv[1:])
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
    setup.write("LICENSE")
    setup.write("README.md")
    for path in sorted(Path("licenses").iterdir()):
        setup.write(path, path.as_posix())
PY
go build -buildvcs=false -trimpath -ldflags '-s -w -H windowsgui' -o build/LocalFileRenamer-Setup-3.3.exe ./installer
python3 - <<'PYZIP'
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile

setup = Path("build/LocalFileRenamer-Setup-3.3.exe")
with ZipFile("build/LocalFileRenamer-Installer-3.3.zip", "w", ZIP_DEFLATED) as archive:
    archive.write(setup, setup.name)
PYZIP
