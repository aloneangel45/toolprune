# ==============================================================================
# toolprune - Official Universal Installer (Windows PowerShell)
# High-Performance Model Context Protocol (MCP) Proxy & Dynamic Schema Pruner
# Repository: https://github.com/aloneangel45/toolprune
# ==============================================================================

$ErrorActionPreference = "Stop"

$Repo = "aloneangel45/toolprune"
$BinaryName = "toolprune.exe"
$InstallDir = Join-Path $env:USERPROFILE ".local\bin"

Write-Host ">>> [INFO] Initiating toolprune Windows installer..." -ForegroundColor Cyan

# 1. Detect Architecture
$Arch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64" -or $env:PROCESSOR_ARCHITEW6432 -eq "ARM64") {
    $Arch = "arm64"
}
Write-Host ">>> [INFO] Detected architecture: Windows/$Arch" -ForegroundColor Gray

# 2. Resolve Latest Release
Write-Host ">>> [INFO] Resolving latest release version from GitHub..." -ForegroundColor Gray
$Tag = "v1.3.1"
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $ReleaseInfo = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing -TimeoutSec 10
    if ($ReleaseInfo.tag_name) {
        $Tag = $ReleaseInfo.tag_name
    }
} catch {
    Write-Host ">>> [WARN] Using default release fallback: $Tag" -ForegroundColor Yellow
}

Write-Host ">>> [INFO] Target release: $Tag" -ForegroundColor Green

# 3. Download Release Asset and Checksums
$ArchiveName = "toolprune-windows-$Arch.zip"
$DownloadUrl = "https://github.com/$Repo/releases/download/$Tag/$ArchiveName"
$ChecksumUrl = "https://github.com/$Repo/releases/download/$Tag/checksums.txt"
$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

try {
    $ZipPath = Join-Path $TempDir $ArchiveName
    $ChecksumPath = Join-Path $TempDir "checksums.txt"

    Write-Host ">>> [INFO] Downloading $DownloadUrl..." -ForegroundColor Gray
    try {
        Invoke-WebRequest -Uri $DownloadUrl -OutFile $ZipPath -UseBasicParsing
    } catch {
        Write-Error "Failed to download toolprune package from $DownloadUrl"
        exit 1
    }

    Write-Host ">>> [INFO] Downloading SHA-256 checksums from $ChecksumUrl..." -ForegroundColor Gray
    try {
        Invoke-WebRequest -Uri $ChecksumUrl -OutFile $ChecksumPath -UseBasicParsing
    } catch {
        Write-Error "Failed to download checksums.txt from $ChecksumUrl"
        exit 1
    }

    # 4. Verify SHA-256 Checksum
    $ExpectedHash = $null
    foreach ($Line in (Get-Content -Path $ChecksumPath)) {
        $Trimmed = $Line.Trim()
        if (-not $Trimmed) { continue }
        $Parts = $Trimmed -split "\s+"
        if ($Parts.Length -ge 2) {
            $FileName = $Parts[-1].TrimStart('*')
            if ($FileName -eq $ArchiveName) {
                $ExpectedHash = $Parts[0].ToLowerInvariant()
                break
            }
        }
    }

    if (-not $ExpectedHash) {
        Write-Error "Checksum entry for $ArchiveName not found in checksums.txt"
        exit 1
    }

    $Sha256 = [System.Security.Cryptography.SHA256]::Create()
    try {
        $FileStream = [System.IO.File]::OpenRead($ZipPath)
        try {
            $HashBytes = $Sha256.ComputeHash($FileStream)
        } finally {
            $FileStream.Dispose()
        }
    } finally {
        $Sha256.Dispose()
    }
    $ActualHash = [System.BitConverter]::ToString($HashBytes).Replace("-", "").ToLowerInvariant()
    if ($ActualHash -ne $ExpectedHash) {
        Write-Error "SHA-256 checksum mismatch for $ArchiveName (expected $ExpectedHash, got $ActualHash)"
        exit 1
    }
    Write-Host ">>> [INFO] SHA-256 checksum verified: $ActualHash" -ForegroundColor Green

    # 5. Extract and Install (using built-in .NET ZipFile for restricted PSModulePath sessions)
    Write-Host ">>> [INFO] Extracting archive..." -ForegroundColor Gray
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $ExtractDir = Join-Path $TempDir "extracted"
    New-Item -ItemType Directory -Path $ExtractDir -Force | Out-Null
    [System.IO.Compression.ZipFile]::ExtractToDirectory($ZipPath, $ExtractDir)

    $ExtractedBinary = Get-ChildItem -Path $ExtractDir -Filter "*.exe" -Recurse | Select-Object -First 1
    if (-not $ExtractedBinary) {
        Write-Error "Extraction failed: Executable not found in archive."
        exit 1
    }

    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    $DestinationFile = Join-Path $InstallDir $BinaryName
    Copy-Item -Path $ExtractedBinary.FullName -Destination $DestinationFile -Force
} finally {
    # 6. Clean up
    Remove-Item -Path $TempDir -Recurse -Force -ErrorAction SilentlyContinue
}

# 7. Ensure PATH is updated in User Environment
$UserPath = [Environment]::GetEnvironmentVariable("PATH", "User")
$NormalizedInstallDir = $InstallDir.TrimEnd('\', '/')
$PathEntries = @()
if ($UserPath) {
    $PathEntries = $UserPath -split ';' | Where-Object { $_ -and $_.Trim() -ne "" }
}
$AlreadyInPath = $false
foreach ($Entry in $PathEntries) {
    if ($Entry.Trim().TrimEnd('\', '/').Equals($NormalizedInstallDir, [System.StringComparison]::OrdinalIgnoreCase)) {
        $AlreadyInPath = $true
        break
    }
}
if (-not $AlreadyInPath) {
    Write-Host ">>> [INFO] Adding $InstallDir to User PATH..." -ForegroundColor Cyan
    if ($PathEntries.Count -gt 0) {
        $NewPath = "$InstallDir;$UserPath"
    } else {
        $NewPath = $InstallDir
    }
    [Environment]::SetEnvironmentVariable("PATH", $NewPath, "User")
    $env:PATH = "$InstallDir;$env:PATH"
}

Write-Host ""
Write-Host "================================================================" -ForegroundColor Green
Write-Host "  toolprune installed successfully to $DestinationFile" -ForegroundColor Green
Write-Host "================================================================" -ForegroundColor Green
Write-Host ""
Write-Host "  Quick Start:" -ForegroundColor White
Write-Host "    toolprune proxy --help                 # View options" -ForegroundColor Yellow
Write-Host "    toolprune proxy --preset balanced      # Run balanced mode" -ForegroundColor Yellow
Write-Host "    toolprune proxy --preset strict        # Maximum token savings" -ForegroundColor Yellow
Write-Host ""
Write-Host "  Documentation: https://github.com/$Repo" -ForegroundColor Gray
Write-Host ""
