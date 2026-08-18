# SBOM Generation and Security Scanning Script
# This script generates SBOM files using Syft and runs vulnerability scanning using Grype

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

function Get-CommandOrFail {
    param([string]$Name)

    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command '$Name' was not found on PATH."
    }
}

function Assert-NativeSuccess {
    param([string]$Message)

    if ($LASTEXITCODE -ne 0) {
        throw $Message
    }
}

$packageName = "obsgateway"
Get-CommandOrFail syft
Get-CommandOrFail grype

# Extract version from obsgateway.yml
$versionLine = Select-String -Path "$packageName.yml" -Pattern "^\s*version:\s*(.+)\s*$" | Select-Object -First 1
if (-not $versionLine) {
    throw "Could not find version in $packageName.yml"
}
$packageVersion = $versionLine.Matches[0].Groups[1].Value.Trim().Trim('"').Trim("'")
if ($packageVersion.StartsWith("v")) {
    $packageVersion = $packageVersion.Substring(1)
}

Write-Host "Generating SBOM and running security scanning..."
Write-Host "Package: $packageName"
Write-Host "Version: $packageVersion"

# Create sbom directory if it doesn't exist
New-Item -ItemType Directory -Force -Path sbom | Out-Null

# Generate SBOM using Syft
Write-Host "Generating SBOM with Syft..."
syft scan dir:. `
  --source-name "$packageName" `
  --source-version "$packageVersion" `
  --exclude ./.git `
  --exclude ./ui/node_modules `
  -o syft-json=sbom/syft.json `
  -o spdx-json=sbom/spdx.json `
  -o syft-text=sbom/sbom.txt `
  -o syft-table=sbom/table.txt

if ($LASTEXITCODE -ne 0) {
    throw "SBOM generation failed."
}

Write-Host "SBOM generation completed successfully."

# Run vulnerability scanning using Grype
Write-Host "Running vulnerability scanning with Grype..."
$oldErrorAction = $ErrorActionPreference
$ErrorActionPreference = "Continue"
grype sbom/syft.json
$grypeExitCode = $LASTEXITCODE
$ErrorActionPreference = $oldErrorAction

if ($grypeExitCode -ne 0) {
    Write-Warning "Security scanning completed with warnings/errors"
} else {
    Write-Host "Security scanning completed successfully"
}