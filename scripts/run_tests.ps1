# run_tests.ps1 — run the Go test suite in a Linux container.
#
# Why Docker rather than a host `go test`: Windows Smart App Control is enforcing
# on this machine and blocks freshly compiled, unsigned test binaries by
# reputation ("An Application Control policy has blocked this file"). Some
# packages happen to pass and others do not, which makes host runs unreliable.
# The project deploys to Linux containers anyway, so running tests there also
# matches the target platform and lets -race work.
#
# Usage:
#   ./scripts/run_tests.ps1                                 # whole suite
#   ./scripts/run_tests.ps1 -Tags integration                # integration suite
#   ./scripts/run_tests.ps1 -Pkg ./internal/radius -V
#   ./scripts/run_tests.ps1 -Race -Count 3 -Tags integration
#   ./scripts/run_tests.ps1 -Cover

param(
    [string]$Pkg = "./...",
    [string]$Run = "",
    [string]$Tags = "",
    [int]$Count = 1,
    [switch]$Race,
    [switch]$V,
    [switch]$Cover,
    [switch]$Vet,
    [string]$Image = "golang:1.22"
)

$ErrorActionPreference = "Continue"
$repo = (Resolve-Path (Split-Path -Parent $PSScriptRoot)).Path
$mount = $repo -replace '\\', '/'

$goArgs = @("test")
if ($Vet) { $goArgs = @("vet") }

if (-not $Vet) {
    if ($Tags)  { $goArgs += "-tags=$Tags" }
    if ($Race)  { $goArgs += "-race" }
    if ($V)     { $goArgs += "-v" }
    if ($Run)   { $goArgs += "-run=$Run" }
    if ($Cover) { $goArgs += "-coverprofile=/src/.coverage.out"; $goArgs += "-covermode=atomic" }
    $goArgs += "-count=$Count"
} elseif ($Tags) {
    $goArgs += "-tags=$Tags"
}
$goArgs += $Pkg

$dockerArgs = @(
    "run", "--rm",
    "-v", "${mount}:/src",
    "-w", "/src",
    "-v", "isp_gomodcache:/go/pkg/mod",
    "-v", "isp_gobuildcache:/root/.cache/go-build",
    "-e", "GOFLAGS=-mod=mod",
    $Image, "go"
) + $goArgs

Write-Host "go $($goArgs -join ' ')" -ForegroundColor Cyan
& docker @dockerArgs
$code = $LASTEXITCODE

if ($code -eq 0) {
    Write-Host "PASS" -ForegroundColor Green
} else {
    Write-Host "FAIL (exit $code)" -ForegroundColor Red
}
exit $code
