param(
    [switch]$SkipRpm,
    [switch]$SkipContainer,
    [switch]$SkipUi
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

function Invoke-Step {
    param(
        [string]$Name,
        [scriptblock]$Script
    )

    Write-Host $Name
    & $Script
    if ($LASTEXITCODE -ne 0) {
        throw "An error has occurred! Aborting the script execution..."
    }
}

function Assert-NativeSuccess {
    param([string]$Message)

    if ($LASTEXITCODE -ne 0) {
        throw $Message
    }
}

function Get-CommandOrFail {
    param([string]$Name)

    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command '$Name' was not found on PATH."
    }
}

$packageName = "obsgateway"
Get-CommandOrFail git
Get-CommandOrFail go
$commit = (git rev-parse HEAD).Trim()
Assert-NativeSuccess "Could not read git commit."
$buildTime = Get-Date -Format "yyyy-MM-ddTHH:mm:sszzz"
$versionLine = Select-String -Path "$packageName.yml" -Pattern "^\s*version:\s*(.+)\s*$" | Select-Object -First 1
if (-not $versionLine) {
    throw "Could not find version in $packageName.yml"
}
$packageVersion = $versionLine.Matches[0].Groups[1].Value.Trim().Trim('"').Trim("'")
if ($packageVersion.StartsWith("v")) {
    $packageVersion = $packageVersion.Substring(1)
}

Write-Host "Building::"
Write-Host " - Version $packageVersion"
Write-Host " - Commit $commit"
Write-Host " - Build Time $buildTime"

if (-not $SkipUi) {
    Get-CommandOrFail npm
    Invoke-Step "Building UI..." {
        Push-Location ui
        try {
            if (Test-Path package-lock.json) {
                npm ci --no-audit --no-fund
            } else {
                Write-Host "No ui/package-lock.json found; using npm install (commit the lock it generates)."
                npm install --no-audit --no-fund
            }
            Assert-NativeSuccess "UI dependency install failed."
            npm run build
            Assert-NativeSuccess "UI build failed."
        } finally {
            Pop-Location
        }
    }
}

if (-not (Test-Path "internal/ui/dist/index.html")) {
    Write-Warning "No UI bundle in internal/ui/dist; /ui/ will report 'ui not built'."
}

New-Item -ItemType Directory -Force -Path build | Out-Null

$oldCGO = $env:CGO_ENABLED
$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
try {
    $env:CGO_ENABLED = "0"
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    $outputName = "$packageName-linux-amd64"

    Invoke-Step "Building $outputName..." {
        go build -a -trimpath -ldflags="-s -w -X main.version=$packageVersion -X main.commit=$commit -X main.buildTime=$buildTime" -o "build/$outputName" .
    }
} finally {
    $env:CGO_ENABLED = $oldCGO
    $env:GOOS = $oldGOOS
    $env:GOARCH = $oldGOARCH
}

Write-Host "Generating SBOM and running security scanning..."
& "$PSScriptRoot/sbom.ps1"

if (-not $SkipRpm) {
    Get-CommandOrFail nfpm
    Invoke-Step "Building RPM..." {
        nfpm package --config "$packageName.yml" --packager rpm --target build/
    }
}

if (-not $SkipContainer) {
    Get-CommandOrFail podman
    Invoke-Step "Building container..." {
        podman build --no-cache -t "${packageName}:latest" .
        Assert-NativeSuccess "Container build failed."
    }

    $archive = "build/$packageName-$packageVersion.tar"
    if (Test-Path $archive) {
        Remove-Item -Force $archive
    }
    Invoke-Step "Saving container archive..." {
        podman save -o $archive "${packageName}:latest"
        Assert-NativeSuccess "Container archive save failed."
    }
}
