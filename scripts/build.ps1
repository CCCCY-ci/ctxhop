[CmdletBinding()]
param(
    [string]$Version = ""
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot ".."))
Set-Location -LiteralPath $repoRoot

if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = (& git describe --tags --always --dirty 2>$null)
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($Version)) {
        $Version = "dev"
    }
}
$commit = (& git rev-parse --short HEAD 2>$null)
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($commit)) {
    $commit = "none"
}
$date = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$package = "github.com/CCCCY-ci/ctxhop/cmd/ctxhop"
$ldflags = "-s -w -X main.version=$Version -X main.commit=$commit -X main.date=$date"

$targets = @(
    @{ OS = "windows"; Arch = "amd64" },
    @{ OS = "windows"; Arch = "arm64" },
    @{ OS = "darwin"; Arch = "amd64" },
    @{ OS = "darwin"; Arch = "arm64" },
    @{ OS = "linux"; Arch = "amd64" },
    @{ OS = "linux"; Arch = "arm64" }
)

$dist = Join-Path $repoRoot "dist"
if (Test-Path -LiteralPath $dist) {
    Remove-Item -LiteralPath $dist -Recurse -Force
}
New-Item -ItemType Directory -Path $dist -Force | Out-Null

foreach ($target in $targets) {
    $suffix = ""
    if ($target.OS -eq "windows") {
        $suffix = ".exe"
    }
$output = Join-Path $dist ("ctxhop_{0}_{1}{2}" -f $target.OS, $target.Arch, $suffix)
    Write-Host ("building {0}/{1}" -f $target.OS, $target.Arch)
    $previousCgo = $env:CGO_ENABLED
    $previousGoOs = $env:GOOS
    $previousGoArch = $env:GOARCH
    try {
        $env:CGO_ENABLED = "0"
        $env:GOOS = $target.OS
        $env:GOARCH = $target.Arch
        & go build -trimpath -ldflags $ldflags -o $output $package
        if ($LASTEXITCODE -ne 0) {
            throw ("go build failed for {0}/{1}" -f $target.OS, $target.Arch)
        }
    }
    finally {
        $env:CGO_ENABLED = $previousCgo
        $env:GOOS = $previousGoOs
        $env:GOARCH = $previousGoArch
    }
}

$hostOs = (& go env GOOS).Trim()
$hostArch = (& go env GOARCH).Trim()
$hostSuffix = ""
if ($hostOs -eq "windows") {
    $hostSuffix = ".exe"
}
$hostOutput = Join-Path $dist ("ctxhop_{0}_{1}{2}" -f $hostOs, $hostArch, $hostSuffix)
if (Test-Path -LiteralPath $hostOutput) {
    Write-Host ("smoke-testing {0}/{1}" -f $hostOs, $hostArch)
    $versionOutput = (& $hostOutput version | Out-String).Trim()
	if ($LASTEXITCODE -ne 0 -or -not $versionOutput.StartsWith("ctxhop ")) {
        throw "host binary failed the version startup smoke test"
    }
    $helpOutput = (& $hostOutput help | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $helpOutput.Contains("commands:")) {
        throw "host binary failed the help startup smoke test"
    }
}
else {
    Write-Host ("host target {0}/{1} is outside the build matrix; startup smoke skipped" -f $hostOs, $hostArch)
}
Write-Host "built $($targets.Count) binaries into $dist"
Get-ChildItem -LiteralPath $dist | Format-Table Name, Length
