# Installs the dock CLI on Windows after verifying its checksum and cosign signature.
#   irm https://raw.githubusercontent.com/usedock/docklet/main/scripts/install.ps1 | iex
# Env: DOCK_VERSION (default: latest release), DOCK_INSTALL_DIR (default: %LOCALAPPDATA%\dock\bin),
#      DOCK_NO_MODIFY_PATH=1 to leave the user PATH alone.
# Self-contained on purpose: the verify logic mirrors scripts/lib-verify.sh. Windows PowerShell 5.1 compatible.

function Install-Dock {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'
  try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch { }

  $cosignVersion = 'v2.4.1'
  $cosignSha256 = '8d57f8a42a981d27290c4227271fa9f0f62ca6630eb4a21d316bd6b01405b87c' # cosign-windows-amd64.exe
  # Only artifacts signed by this repo's release workflow at the exact release tag are accepted.
  $identityPrefix = 'https://github.com/usedock/docklet/.github/workflows/release.yml@refs/tags/'
  $oidcIssuer = 'https://token.actions.githubusercontent.com'

  function Fail($msg) { throw "install: $msg" }
  function Test-Version($v) {
    return ($v -cmatch '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?\z')
  }
  function Get-Sha256($path) { return (Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash.ToLowerInvariant() }
  function Get-RecordedChecksum($sums, $name) {
    foreach ($line in (Get-Content -LiteralPath $sums)) {
      $parts = $line.Trim() -split '\s+'
      if ($parts.Count -ge 2 -and $parts[1].TrimStart('*') -ceq $name) { return $parts[0] }
    }
    return $null
  }
  function Confirm-Checksum($file, $sums, $name) {
    if (-not (Test-Path -LiteralPath $file)) { Fail "missing file to verify: $file" }
    if (-not (Test-Path -LiteralPath $sums)) { Fail "missing checksums file: $sums" }
    $want = Get-RecordedChecksum $sums $name
    if (-not $want) { Fail "no checksum for $name in checksums.txt" }
    $got = Get-Sha256 $file
    if ($got -ne $want.ToLowerInvariant()) { Fail "checksum mismatch for ${name}: expected $want, got $got" }
  }
  function Confirm-Signature($cosign, $file, $bundle, $version) {
    if (-not (Test-Version $version)) { Fail "invalid release version '$version'; expected v<major>.<minor>.<patch>" }
    if (-not $cosign -or -not (Test-Path -LiteralPath $cosign)) { Fail "cosign is not available; cannot verify $file" }
    if (-not (Test-Path -LiteralPath $bundle)) { Fail "missing signature bundle for ${file}: $bundle" }
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
      $out = & $cosign verify-blob --bundle $bundle --certificate-identity "$identityPrefix$version" --certificate-oidc-issuer $oidcIssuer $file 2>&1
      $code = $LASTEXITCODE
    } finally { $ErrorActionPreference = $prev }
    if ($code -ne 0) {
      ($out | Out-String).Trim() | Write-Host
      Fail "signature verification FAILED for $file"
    }
  }
  function Get-Cosign($dir) {
    # A preinstalled cosign is used only when its GitVersion is exactly the pinned version.
    $have = Get-Command cosign -ErrorAction SilentlyContinue
    if ($have) {
      $prev = $ErrorActionPreference
      $ErrorActionPreference = 'Continue'
      try { $out = (& $have.Source version 2>$null | Out-String) } catch { $out = '' } finally { $ErrorActionPreference = $prev }
      foreach ($l in ($out -split "`n")) {
        $f = $l.Trim() -split '\s+'
        if ($f.Count -eq 2 -and $f[0] -ceq 'GitVersion:' -and $f[1] -ceq $cosignVersion) { return $have.Source }
      }
    }
    $dest = Join-Path $dir 'cosign.exe'
    Invoke-WebRequest -UseBasicParsing -Uri "https://github.com/sigstore/cosign/releases/download/$cosignVersion/cosign-windows-amd64.exe" -OutFile $dest
    if ((Get-Sha256 $dest) -ne $cosignSha256) { Fail "cosign $cosignVersion checksum mismatch; refusing to run it" }
    return $dest
  }

  $archRaw = $env:DOCK_ARCH
  if (-not $archRaw) { $archRaw = $env:PROCESSOR_ARCHITEW6432 }
  if (-not $archRaw) { $archRaw = $env:PROCESSOR_ARCHITECTURE }
  switch -Regex ($archRaw) {
    '^(AMD64|x86_64|amd64)$' { $arch = 'amd64'; break }
    '^(ARM64|arm64|aarch64)$' { $arch = 'arm64'; break }
    default { Fail "unsupported architecture: $archRaw" }
  }

  $version = $env:DOCK_VERSION
  if (-not $version) {
    $api = $env:DOCK_LATEST_URL
    if (-not $api) { $api = 'https://api.github.com/repos/usedock/docklet/releases/latest' }
    try { $version = (Invoke-RestMethod -UseBasicParsing -Uri $api).tag_name } catch { Fail "could not resolve the latest release from ${api}: $($_.Exception.Message)" }
  }
  if (-not (Test-Version $version)) { Fail "invalid release version '$version'; expected v<major>.<minor>.<patch>" }

  $asset = "dock-windows-$arch.exe"
  $baseUrl = $env:DOCK_RELEASE_BASE_URL
  if (-not $baseUrl) { $baseUrl = 'https://github.com/usedock/docklet/releases/download' }
  $base = "$baseUrl/$version"
  $dest = $env:DOCK_INSTALL_DIR
  if (-not $dest) { $dest = Join-Path $env:LOCALAPPDATA 'dock\bin' }

  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("dock-install-" + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    Write-Host "install: downloading $asset $version"
    foreach ($f in @($asset, 'checksums.txt', 'checksums.txt.bundle', "$asset.bundle")) {
      try { Invoke-WebRequest -UseBasicParsing -Uri "$base/$f" -OutFile (Join-Path $tmp $f) } catch { Fail "could not download $f from ${base}: $($_.Exception.Message)" }
    }
    $cosign = Get-Cosign $tmp
    Confirm-Signature $cosign (Join-Path $tmp 'checksums.txt') (Join-Path $tmp 'checksums.txt.bundle') $version
    Confirm-Signature $cosign (Join-Path $tmp $asset) (Join-Path $tmp "$asset.bundle") $version
    Confirm-Checksum (Join-Path $tmp $asset) (Join-Path $tmp 'checksums.txt') $asset

    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    Copy-Item -LiteralPath (Join-Path $tmp $asset) -Destination (Join-Path $dest 'dock.exe') -Force
    Write-Host "install: installed dock $version to $(Join-Path $dest 'dock.exe')"
  } finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }

  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  $entries = @()
  if ($userPath) { $entries = $userPath -split ';' | Where-Object { $_ } }
  if ($entries -notcontains $dest) {
    if ($env:DOCK_NO_MODIFY_PATH -eq '1') {
      Write-Host "install: $dest is not on your PATH; add it to use 'dock' from any shell"
    } else {
      [Environment]::SetEnvironmentVariable('Path', ((@($entries) + $dest) -join ';'), 'User')
      Write-Host "install: added $dest to your user PATH; open a new terminal to use 'dock'"
    }
  }
}

Install-Dock
