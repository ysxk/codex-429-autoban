# Build the codex-429-autoban plugin into a Windows .dll.
#
# Requirements:
#   - Go (this plugin needs Go 1.21+; it vendors the CPA SDK locally so it
#     does NOT require the Go 1.26 that the full CLIProxyAPI module needs)
#   - A C compiler (CGO is mandatory for CPA plugins). On Windows install
#     MinGW-w64, e.g. via winget:
#         winget install -e --id MartinStorsjo.LLVM-MinGW.UCRT
#     or TDM-GCC, or MSYS2's mingw-w64-x86_64-gcc. Make sure gcc.exe is on PATH.
#
# After a successful build, copy the .dll into your CPA plugins directory:
#         plugins/windows/amd64/codex-429-autoban.dll
#   (CPA also accepts it directly under plugins/ . The plugin id is the file
#    name without the extension: "codex-429-autoban".)

$ErrorActionPreference = "Stop"

$out = "codex-429-autoban.dll"
Write-Host "Building $out (CGO c-shared)..."
$env:CGO_ENABLED = "1"
go build -buildmode=c-shared -o $out .

if ($LASTEXITCODE -ne 0) {
    Write-Error "Build failed. Ensure Go and a C compiler (gcc) are installed and on PATH."
    exit 1
}

Write-Host ""
Write-Host "Built: $((Resolve-Path $out).Path)"
Write-Host "Next: copy it to <cpa>/plugins/windows/amd64/$out"
Write-Host "      and enable it in config.yaml (see README.md)."
