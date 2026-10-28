# Installs selfaction from its GitHub releases on Windows (PowerShell 5.1 or 7):
#
#   irm https://raw.githubusercontent.com/broisnees/selfaction/main/install.ps1 | iex
#
# $env:SELFACTION_VERSION      a release tag such as v0.3.0 (default: the latest release)
# $env:SELFACTION_INSTALL_DIR  where to put selfaction.exe
#                              (default: %LOCALAPPDATA%\Programs\selfaction, added to your PATH)
#
# The archive's SHA-256 is checked against the release's checksums.txt.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'broisnees/selfaction'
$base = if ($env:SELFACTION_DOWNLOAD_URL) { $env:SELFACTION_DOWNLOAD_URL } else { "https://github.com/$repo/releases" }
$version = if ($env:SELFACTION_VERSION) { $env:SELFACTION_VERSION } else { 'latest' }
$dir = if ($env:SELFACTION_INSTALL_DIR) { $env:SELFACTION_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\selfaction' }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "unsupported architecture $($env:PROCESSOR_ARCHITECTURE)" }
}

if ($version -eq 'latest') {
    $version = (Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$repo/releases/latest").tag_name
    if (-not $version) { throw "no release found for $repo" }
}

$name = "selfaction_$($version.TrimStart('v'))_windows_$arch"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "downloading selfaction $version for windows/$arch"
    $zip = Join-Path $tmp "$name.zip"
    Invoke-WebRequest -UseBasicParsing "$base/download/$version/$name.zip" -OutFile $zip
    $sums = (Invoke-WebRequest -UseBasicParsing "$base/download/$version/checksums.txt").Content
    if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }

    $want = $null
    foreach ($line in ($sums -split "`n")) {
        $parts = $line.Trim() -split '\s+'
        if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq "$name.zip") { $want = $parts[0] }
    }
    if (-not $want) { throw "$name.zip is not in checksums.txt" }
    $got = (Get-FileHash -Algorithm SHA256 $zip).Hash
    if ($got -ne $want.ToUpperInvariant()) { throw "checksum mismatch for $name.zip" }

    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    Copy-Item -Force (Join-Path $tmp "$name\selfaction.exe") (Join-Path $dir 'selfaction.exe')
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$exe = Join-Path $dir 'selfaction.exe'
Write-Host "installed $(& $exe --version) to $exe"

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not (($userPath -split ';') -contains $dir)) {
    [Environment]::SetEnvironmentVariable('Path', (($userPath, $dir) | Where-Object { $_ }) -join ';', 'User')
    Write-Host "added $dir to your PATH; open a new terminal to run selfaction"
}
