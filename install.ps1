# ==============================================================================
# toolprune - Official Universal Installer (Windows PowerShell)
# High-Performance Model Context Protocol (MCP) Proxy & Dynamic Schema Pruner
# Repository: https://github.com/aloneangel45/toolprune
# ==============================================================================

function Install-ToolPrune {
    $ErrorActionPreference = "Stop"

    $Repo = "aloneangel45/toolprune"
    $BinaryName = "toolprune.exe"

    $InstallDir = $env:TOOLPRUNE_INSTALL_DIR
    if ([string]::IsNullOrWhiteSpace($InstallDir)) {
        $BaseProfile = $env:USERPROFILE
        if ([string]::IsNullOrWhiteSpace($BaseProfile)) {
            $BaseProfile = [Environment]::GetFolderPath("UserProfile")
        }
        $InstallDir = [System.IO.Path]::Combine($BaseProfile, ".local", "bin")
    }

    [Console]::WriteLine(">>> [INFO] Initiating toolprune Windows installer...")

    # 1. Detect Architecture
    $Arch = "amd64"
    if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64" -or $env:PROCESSOR_ARCHITEW6432 -eq "ARM64") {
        $Arch = "arm64"
    }
    [Console]::WriteLine(">>> [INFO] Detected architecture: Windows/$Arch")

    # 2. Configure TLS 1.2+ without clobbering TLS 1.3 and Resolve Release
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $Tag = "v1.4.0"
    $AssetDir = $env:TOOLPRUNE_ASSET_DIR
    if ([string]::IsNullOrWhiteSpace($AssetDir)) {
        [Console]::WriteLine(">>> [INFO] Resolving latest release version from GitHub...")
        try {
            $ApiClient = [System.Net.WebClient]::new()
            try {
                $ApiClient.Headers.Add("User-Agent", "toolprune-installer")
                $ReleaseJson = $ApiClient.DownloadString("https://api.github.com/repos/$Repo/releases/latest")
                $TagMatch = [System.Text.RegularExpressions.Regex]::Match($ReleaseJson, '"tag_name"\s*:\s*"([^"]+)"')
                if ($TagMatch.Success -and -not [string]::IsNullOrWhiteSpace($TagMatch.Groups[1].Value)) {
                    $Tag = $TagMatch.Groups[1].Value
                }
            } finally {
                $ApiClient.Dispose()
            }
        } catch {
            [Console]::WriteLine(">>> [WARN] Using default release fallback: $Tag")
        }
    }

    [Console]::WriteLine(">>> [INFO] Target release: $Tag")

    # 3. Acquire Release Asset and Checksums
    $ArchiveName = "toolprune-windows-$Arch.zip"
    $DownloadUrl = "https://github.com/$Repo/releases/download/$Tag/$ArchiveName"
    $ChecksumUrl = "https://github.com/$Repo/releases/download/$Tag/checksums.txt"
    $TempDir = [System.IO.Path]::Combine([System.IO.Path]::GetTempPath(), ("toolprune-install-" + [System.Guid]::NewGuid().ToString("N")))
    [void][System.IO.Directory]::CreateDirectory($TempDir)

    $DestinationFile = [System.IO.Path]::Combine($InstallDir, $BinaryName)

    try {
        $ZipPath = [System.IO.Path]::Combine($TempDir, $ArchiveName)
        $ChecksumPath = [System.IO.Path]::Combine($TempDir, "checksums.txt")

        if (-not [string]::IsNullOrWhiteSpace($AssetDir)) {
            $LocalZip = [System.IO.Path]::Combine($AssetDir, $ArchiveName)
            $LocalChecksum = [System.IO.Path]::Combine($AssetDir, "checksums.txt")
            if (-not [System.IO.File]::Exists($LocalZip)) {
                throw "Local release archive not found at $LocalZip"
            }
            if (-not [System.IO.File]::Exists($LocalChecksum)) {
                throw "Local checksums.txt not found at $LocalChecksum"
            }
            [System.IO.File]::Copy($LocalZip, $ZipPath, $true)
            [System.IO.File]::Copy($LocalChecksum, $ChecksumPath, $true)
        } else {
            [Console]::WriteLine(">>> [INFO] Downloading $DownloadUrl...")
            $WebClient = [System.Net.WebClient]::new()
            try {
                $WebClient.Headers.Add("User-Agent", "toolprune-installer")
                try {
                    $WebClient.DownloadFile($DownloadUrl, $ZipPath)
                } catch {
                    throw "Failed to download toolprune package from ${DownloadUrl}: $($_.Exception.Message)"
                }
                [Console]::WriteLine(">>> [INFO] Downloading SHA-256 checksums from $ChecksumUrl...")
                try {
                    $WebClient.DownloadFile($ChecksumUrl, $ChecksumPath)
                } catch {
                    throw "Failed to download checksums.txt from ${ChecksumUrl}: $($_.Exception.Message)"
                }
            } finally {
                $WebClient.Dispose()
            }
        }

        # 4. Verify SHA-256 Checksum (pure .NET)
        $ExpectedHash = $null
        foreach ($Line in [System.IO.File]::ReadAllLines($ChecksumPath)) {
            $Trimmed = $Line.Trim()
            if ([string]::IsNullOrEmpty($Trimmed)) { continue }
            $Parts = $Trimmed -split "\s+"
            if ($Parts.Length -ge 2) {
                $FileName = $Parts[$Parts.Length - 1].TrimStart('*')
                if ($FileName -eq $ArchiveName) {
                    $ExpectedHash = $Parts[0].ToLowerInvariant()
                    break
                }
            }
        }

        if ([string]::IsNullOrEmpty($ExpectedHash)) {
            throw "Checksum entry for $ArchiveName not found in checksums.txt"
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
            throw "SHA-256 checksum mismatch for $ArchiveName (expected $ExpectedHash, got $ActualHash)"
        }
        [Console]::WriteLine(">>> [INFO] SHA-256 checksum verified: $ActualHash")

        # 5. Extract and Install (pure .NET without PowerShell module autoloading)
        [Console]::WriteLine(">>> [INFO] Extracting archive...")
        if (-not ("System.IO.Compression.ZipFile" -as [type])) {
            $LoadedAssembly = [System.Reflection.Assembly]::LoadWithPartialName("System.IO.Compression.FileSystem")
            if (-not $LoadedAssembly -and -not ("System.IO.Compression.ZipFile" -as [type])) {
                throw "Failed to load System.IO.Compression.FileSystem assembly."
            }
        }
        $ExtractDir = [System.IO.Path]::Combine($TempDir, "extracted")
        [void][System.IO.Directory]::CreateDirectory($ExtractDir)
        [System.IO.Compression.ZipFile]::ExtractToDirectory($ZipPath, $ExtractDir)

        $ExtractedExecutables = [System.IO.Directory]::GetFiles($ExtractDir, "*.exe", [System.IO.SearchOption]::AllDirectories)
        if (-not $ExtractedExecutables -or $ExtractedExecutables.Length -eq 0) {
            throw "Extraction failed: Executable not found in archive."
        }
        $ExtractedBinary = $ExtractedExecutables[0]

        if (-not [System.IO.Directory]::Exists($InstallDir)) {
            [void][System.IO.Directory]::CreateDirectory($InstallDir)
        }

        if ([System.IO.File]::Exists($DestinationFile)) {
            $ExistingAttrs = [System.IO.File]::GetAttributes($DestinationFile)
            if (($ExistingAttrs -band [System.IO.FileAttributes]::ReadOnly) -ne 0) {
                [System.IO.File]::SetAttributes($DestinationFile, ($ExistingAttrs -band (-bnot [System.IO.FileAttributes]::ReadOnly)))
            }
        }
        [System.IO.File]::Copy($ExtractedBinary, $DestinationFile, $true)
    } finally {
        # 6. Clean up temporary directory
        if ([System.IO.Directory]::Exists($TempDir)) {
            try {
                [System.IO.Directory]::Delete($TempDir, $true)
            } catch {
                # Ignore transient antivirus file-lock on temp cleanup
            }
        }
    }

    # 7. Ensure PATH is updated in User Environment
    if ($env:TOOLPRUNE_SKIP_PATH_UPDATE -ne "1") {
        $UserPath = [Environment]::GetEnvironmentVariable("PATH", "User")
        $NormalizedInstallDir = $InstallDir.TrimEnd('\', '/')
        $NonEmptyEntries = 0
        $AlreadyInPath = $false
        if (-not [string]::IsNullOrEmpty($UserPath)) {
            foreach ($RawEntry in $UserPath.Split(';')) {
                if (-not [string]::IsNullOrWhiteSpace($RawEntry)) {
                    $NonEmptyEntries++
                    if ($RawEntry.Trim().TrimEnd('\', '/').Equals($NormalizedInstallDir, [System.StringComparison]::OrdinalIgnoreCase)) {
                        $AlreadyInPath = $true
                        break
                    }
                }
            }
        }
        if (-not $AlreadyInPath) {
            [Console]::WriteLine(">>> [INFO] Adding $InstallDir to User PATH...")
            if ($NonEmptyEntries -gt 0) {
                $NewPath = "$InstallDir;$UserPath"
            } else {
                $NewPath = $InstallDir
            }
            [Environment]::SetEnvironmentVariable("PATH", $NewPath, "User")
            $env:PATH = "$InstallDir;$env:PATH"
        }
    }

    [Console]::WriteLine("")
    [Console]::WriteLine("================================================================")
    [Console]::WriteLine("  toolprune installed successfully to $DestinationFile")
    [Console]::WriteLine("================================================================")
    [Console]::WriteLine("")
    [Console]::WriteLine("  Quick Start:")
    [Console]::WriteLine("    toolprune proxy --help                 # View options")
    [Console]::WriteLine("    toolprune proxy --preset balanced      # Run balanced mode")
    [Console]::WriteLine("    toolprune proxy --preset strict        # Maximum token savings")
    [Console]::WriteLine("")
    [Console]::WriteLine("  Documentation: https://github.com/$Repo")
    [Console]::WriteLine("")
}

Install-ToolPrune
