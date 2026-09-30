# ==============================================================================
# 絆 KIZUNA - Installer for Windows (PowerShell)
# Repository: https://github.com/osuki-dev/kizuna
# ==============================================================================
[CmdletBinding()]
param (
    [string]$Version = $env:KIZUNA_VERSION,
    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\Kizuna\bin",
    [switch]$InstallService = ($env:INSTALL_SERVICE -eq "true" -or $env:INSTALL_SERVICE -eq "yes")
)

$ErrorActionPreference = "Stop"
$GitHubRepo = "osuki-dev/kizuna"

Write-Host " _  ___                     " -ForegroundColor Cyan
Write-Host "| |/ (_)____  _ _ _  __ _   " -ForegroundColor Cyan
Write-Host "| ' <| |_ / || | ' \/ _` |  " -ForegroundColor Cyan
Write-Host "|_|\_\_/__|\_,_|_||_\__,_|  " -ForegroundColor Cyan
Write-Host " 絆 KIZUNA - Windows Installer" -ForegroundColor Green
Write-Host ""

# 1. Detect Architecture
$Arch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") {
    $Arch = "arm64"
}
Write-Host "==> Detected architecture: Windows/$Arch" -ForegroundColor Cyan

# 2. Determine Version
if (-not $Version) {
    Write-Host "==> Fetching latest release tag from GitHub..." -ForegroundColor Cyan
    try {
        $ReleaseUrl = "https://api.github.com/repos/$GitHubRepo/releases/latest"
        $Release = Invoke-RestMethod -Uri $ReleaseUrl -Headers @{ "User-Agent" = "kizuna-installer" }
        $Version = $Release.tag_name
    } catch {
        $Version = "v0.3.0"
        Write-Warning "Could not fetch latest release, using default $Version"
    }
}
Write-Host "==> Target version: $Version" -ForegroundColor Cyan

# 3. Prepare Download
$ArchiveName = "kizuna_${Version}_windows_${Arch}.zip"
$DownloadUrl = "https://github.com/$GitHubRepo/releases/download/$Version/$ArchiveName"
$ChecksumsUrl = "https://github.com/$GitHubRepo/releases/download/$Version/checksums.txt"

$TempDir = Join-Path $env:TEMP ([System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null
$ZipPath = Join-Path $TempDir $ArchiveName
$ChecksumsPath = Join-Path $TempDir "checksums.txt"

try {
    Write-Host "==> Downloading $ArchiveName..." -ForegroundColor Cyan
    try {
        Invoke-WebRequest -Uri $DownloadUrl -OutFile $ZipPath -UseBasicParsing
    } catch {
        # Fallback to direct exe
        $ArchiveName = "kizuna-windows-$Arch.exe"
        $DownloadUrl = "https://github.com/$GitHubRepo/releases/download/$Version/$ArchiveName"
        Write-Host "==> Trying direct binary: $DownloadUrl..." -ForegroundColor Yellow
        $ZipPath = Join-Path $TempDir "kizuna.exe"
        Invoke-WebRequest -Uri $DownloadUrl -OutFile $ZipPath -UseBasicParsing
    }

    # 4. SHA256 Verification
    Write-Host "==> Verifying SHA256 checksum..." -ForegroundColor Cyan
    try {
        Invoke-WebRequest -Uri $ChecksumsUrl -OutFile $ChecksumsPath -UseBasicParsing
        if (Test-Path $ChecksumsPath) {
            $ChecksumContent = Get-Content $ChecksumsPath -Raw
            $ActualHash = (Get-FileHash -Path $ZipPath -Algorithm SHA256).Hash.ToLower()

            $Matched = $false
            foreach ($line in ($ChecksumContent -split "`r?`n")) {
                if ($line -match "^([a-fA-F0-9]{64})\s+(.+)$") {
                    $hash = $matches[1].ToLower()
                    $file = [System.IO.Path]::GetFileName($matches[2])
                    if ($file -eq $ArchiveName) {
                        $Matched = $true
                        if ($ActualHash -ne $hash) {
                            throw "SHA256 checksum mismatch! Expected $hash, got $ActualHash"
                        }
                        Write-Host "✓ SHA256 checksum verified: $($ActualHash.Substring(0, 16))..." -ForegroundColor Green
                        break
                    }
                }
            }
            if (-not $Matched) {
                Write-Warning "Archive not found in checksums.txt, skipping check."
            }
        }
    } catch {
        Write-Warning "Could not perform checksum verification: $_"
    }

    # 5. Extract Binary
    if ($ZipPath.EndsWith(".zip")) {
        Write-Host "==> Extracting $ArchiveName..." -ForegroundColor Cyan
        Expand-Archive -Path $ZipPath -DestinationPath $TempDir -Force
    }

    $SrcExe = Join-Path $TempDir "kizuna.exe"
    if (-not (Test-Path $SrcExe)) {
        $found = Get-ChildItem -Path $TempDir -Filter "kizuna*.exe" -Recurse | Select-Object -First 1
        if ($found) {
            $SrcExe = $found.FullName
        } else {
            throw "kizuna.exe not found in downloaded package."
        }
    }

    # 6. Install to destination
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $DestExe = Join-Path $InstallDir "kizuna.exe"
    Copy-Item -Path $SrcExe -Destination $DestExe -Force
    Write-Host "✓ Installed kizuna.exe to $DestExe" -ForegroundColor Green

    # 7. Add to User PATH if not present
    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($UserPath -notlike "*$InstallDir*") {
        Write-Host "==> Adding $InstallDir to User PATH..." -ForegroundColor Cyan
        [Environment]::SetEnvironmentVariable("Path", "$UserPath;$InstallDir", "User")
        $env:Path = "$env:Path;$InstallDir"
        Write-Host "✓ Added to PATH" -ForegroundColor Green
    }

    # 8. Check Service Installation
    $PromptService = $false
    if (-not $InstallService -and [Environment]::UserInteractive) {
        $response = Read-Host "? Do you want to install and start Kizuna as a Windows background service? [y/N]"
        if ($response -match "^[yY]([eE][sS])?$") {
            $InstallService = $true
        }
    }

    if ($InstallService) {
        Write-Host "==> Installing Kizuna Windows Service..." -ForegroundColor Cyan
        & "$DestExe" service install
        Write-Host "✓ Kizuna service installed!" -ForegroundColor Green
    } else {
        Write-Host "  Tip: Run 'kizuna service install' anytime to enable background daemon." -ForegroundColor Gray
    }

    Write-Host ""
    Write-Host "★ Kizuna installation complete!" -ForegroundColor Green
    Write-Host "  Run 'kizuna --help' to get started"
    Write-Host "  Run 'kizuna init' to initialize a configuration file"
} finally {
    Remove-Item -Path $TempDir -Recurse -Force -ErrorAction SilentlyContinue
}
