# Runs scripts/install.ps1 against a local file:// fake release and a fake cosign; run in CI (pwsh) or locally.
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$installer = Join-Path $here 'install.ps1'
$psExe = (Get-Process -Id $PID).Path
$onWindows = [IO.Path]::DirectorySeparatorChar -eq '\'
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("dock-test-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
$script:fail = 0

function Write-Lf($path, $text) { [IO.File]::WriteAllText($path, $text.Replace("`r`n", "`n")) }

# Fake cosign reports the pinned version; mode ok accepts, bad rejects, tag accepts only COSIGN_EXPECT_TAG.
$bin = Join-Path $tmp 'bin'
New-Item -ItemType Directory -Path $bin | Out-Null
function Set-FakeCosign($mode) {
  if ($onWindows) {
    $body = "@echo off`r`nif `"%1`"==`"version`" (echo GitVersion:    v2.4.1& exit /b 0)`r`n"
    if ($mode -eq 'ok') { $body += "exit /b 0`r`n" }
    elseif ($mode -eq 'bad') { $body += "exit /b 1`r`n" }
    else { $body += "echo %* | findstr /C:`"release.yml@refs/tags/%COSIGN_EXPECT_TAG% `" >nul && exit /b 0`r`nexit /b 1`r`n" }
    [IO.File]::WriteAllText((Join-Path $bin 'cosign.cmd'), $body)
  } else {
    $body = "#!/bin/sh`n[ `"`$1`" = version ] && { echo `"GitVersion:    v2.4.1`"; exit 0; }`n"
    if ($mode -eq 'ok') { $body += "exit 0`n" }
    elseif ($mode -eq 'bad') { $body += "exit 1`n" }
    else { $body += "case `" `$* `" in *`"release.yml@refs/tags/`$COSIGN_EXPECT_TAG `"*) exit 0 ;; esac`nexit 1`n" }
    $p = Join-Path $bin 'cosign'
    Write-Lf $p $body
    chmod +x $p
  }
}

$rel = Join-Path $tmp 'rel/v0.0.1'
New-Item -ItemType Directory -Path $rel -Force | Out-Null
$asset = 'dock-windows-amd64.exe'
[IO.File]::WriteAllText((Join-Path $rel $asset), 'dock-cli-bytes')
function Write-Sums($name) {
  $h = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $rel $asset)).Hash.ToLowerInvariant()
  Write-Lf (Join-Path $rel 'checksums.txt') "$h  $name`n"
}
Write-Sums $asset
foreach ($b in @('checksums.txt.bundle', "$asset.bundle")) { [IO.File]::WriteAllText((Join-Path $rel $b), '') }
# Serve the fake release over loopback http (pwsh on Linux cannot fetch file:// URLs).
$port = Get-Random -Minimum 20000 -Maximum 40000
$relUrl = "http://127.0.0.1:$port"
$server = Start-Job -ArgumentList (Join-Path $tmp 'rel'), $port -ScriptBlock {
  param($root, $port)
  $l = New-Object System.Net.HttpListener
  $l.Prefixes.Add("http://127.0.0.1:$port/")
  $l.Start()
  while ($l.IsListening) {
    $c = $l.GetContext()
    $f = Join-Path $root ($c.Request.Url.AbsolutePath.TrimStart('/') -replace '/', [IO.Path]::DirectorySeparatorChar)
    if ((Test-Path -LiteralPath $f -PathType Leaf) -and $f.StartsWith($root)) {
      $b = [IO.File]::ReadAllBytes($f)
      $c.Response.OutputStream.Write($b, 0, $b.Length)
    } else { $c.Response.StatusCode = 404 }
    $c.Response.Close()
  }
}
for ($i = 0; $i -lt 50; $i++) {
  try { [void](New-Object Net.Sockets.TcpClient('127.0.0.1', $port)).Close(); break } catch { Start-Sleep -Milliseconds 200 }
}

$script:n = 0
# Runs install.ps1 in a child process; returns the exit code and sets $script:dest and $script:out.
function Invoke-Install($mode, $extra = @{}) {
  Set-FakeCosign $mode
  $script:n++
  $script:dest = Join-Path $tmp "dest$($script:n)"
  $saved = @{}
  $vars = @{
    DOCK_ARCH = 'amd64'; DOCK_VERSION = 'v0.0.1'; DOCK_RELEASE_BASE_URL = $relUrl
    DOCK_INSTALL_DIR = $script:dest; DOCK_NO_MODIFY_PATH = '1'; COSIGN_EXPECT_TAG = 'v0.0.1'
  }
  foreach ($k in $extra.Keys) { $vars[$k] = $extra[$k] }
  foreach ($k in $vars.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); Set-Item -Path "env:$k" -Value $vars[$k] }
  $oldPath = $env:PATH
  $env:PATH = $bin + [IO.Path]::PathSeparator + $env:PATH
  $ErrorActionPreference = 'Continue'
  try {
    $script:out = & $psExe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $installer 2>&1 | Out-String
    return $LASTEXITCODE
  } finally {
    $ErrorActionPreference = 'Stop'
    $env:PATH = $oldPath
    foreach ($k in $saved.Keys) { if ($null -eq $saved[$k]) { Remove-Item "env:$k" -ErrorAction SilentlyContinue } else { Set-Item -Path "env:$k" -Value $saved[$k] } }
  }
}
function Expect($name, $cond) {
  if ($cond) { Write-Host "ok: $name" } else { Write-Host "FAIL: $name"; Write-Host $script:out; $script:fail = 1 }
}
function Expect-Install($name, $mode, $extra = @{}) {
  $rc = Invoke-Install $mode $extra
  $exe = Join-Path $script:dest 'dock.exe'
  Expect $name (($rc -eq 0) -and (Test-Path $exe) -and ([IO.File]::ReadAllText($exe) -eq 'dock-cli-bytes'))
}
# $reason is a regex the refusal message must match, so a refusal cannot pass for the wrong cause.
function Expect-Refuse($name, $mode, $reason, $extra = @{}) {
  $rc = Invoke-Install $mode $extra
  Expect "$name (refused)" (($rc -ne 0) -and ($script:out -match $reason) -and -not (Test-Path (Join-Path $script:dest 'dock.exe')))
}

Expect-Install 'good install' 'ok'
Expect-Install 'good install, exact identity' 'tag'
Expect-Refuse 'wrong tag' 'tag' 'signature verification FAILED' @{ COSIGN_EXPECT_TAG = 'v0.0.2' }
Expect-Refuse 'bad signature' 'bad' 'signature verification FAILED'
Expect-Refuse 'invalid version' 'ok' 'invalid release version' @{ DOCK_VERSION = 'v0.0.1/../x' }
Expect-Refuse 'unprefixed version' 'ok' 'invalid release version' @{ DOCK_VERSION = '0.0.1' }
Expect-Refuse 'unsupported arch' 'ok' 'unsupported architecture' @{ DOCK_ARCH = 'riscv64' }

$orig = [IO.File]::ReadAllBytes((Join-Path $rel $asset))
[IO.File]::WriteAllText((Join-Path $rel $asset), 'tampered')
Expect-Refuse 'tampered binary' 'ok' 'checksum mismatch'
[IO.File]::WriteAllBytes((Join-Path $rel $asset), $orig)
Write-Sums 'other-file'
Expect-Refuse 'no checksum entry' 'ok' 'no checksum for'
Write-Sums $asset
Remove-Item (Join-Path $rel "$asset.bundle")
Expect-Refuse 'missing binary bundle' 'ok' 'could not download'
[IO.File]::WriteAllText((Join-Path $rel "$asset.bundle"), '')
Remove-Item (Join-Path $rel 'checksums.txt.bundle')
Expect-Refuse 'missing checksums bundle' 'ok' 'could not download'
[IO.File]::WriteAllText((Join-Path $rel 'checksums.txt.bundle'), '')
Remove-Item (Join-Path $rel 'checksums.txt')
Expect-Refuse 'missing checksums.txt' 'ok' 'could not download'
Write-Sums $asset
Expect-Install 'good install again' 'ok'

# Latest release is resolved from the API response and must be a single valid tag.
Write-Lf (Join-Path $tmp 'rel/latest.json') "{`"tag_name`": `"v0.0.1`"}"
Write-Lf (Join-Path $tmp 'rel/latest-array.json') "[{`"tag_name`": `"junk`"}, {`"tag_name`": `"v0.0.1`"}]"
Expect-Install 'latest release' 'ok' @{ DOCK_VERSION = ''; DOCK_LATEST_URL = "$relUrl/latest.json" }
Expect-Refuse 'latest as array' 'ok' 'invalid release version' @{ DOCK_VERSION = ''; DOCK_LATEST_URL = "$relUrl/latest-array.json" }

# Add-UserPath keeps %VAR% entries and REG_EXPAND_SZ, and is idempotent (scratch key, never the real PATH).
if ($onWindows) {
  $ast = [System.Management.Automation.Language.Parser]::ParseFile($installer, [ref]$null, [ref]$null)
  $fn = $ast.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Add-UserPath' }, $true)
  . ([scriptblock]::Create($fn.Extent.Text))
  $scratch = 'Software\DockInstallTest-' + [Guid]::NewGuid().ToString('N')
  $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($scratch)
  try {
    $key.SetValue('Path', '%USERPROFILE%\go\bin;%DOCK_UNDEFINED_VAR%\bin', [Microsoft.Win32.RegistryValueKind]::ExpandString)
    $first = Add-UserPath $key 'C:\dock\bin'
    $second = Add-UserPath $key 'C:\dock\bin\'
    $third = Add-UserPath $key ([Environment]::ExpandEnvironmentVariables('%USERPROFILE%\go\bin'))
    $raw = $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    $script:out = "raw=$raw kind=$($key.GetValueKind('Path')) first=$first second=$second third=$third"
    Expect 'user PATH keeps %VAR% entries and type' (($raw -ceq '%USERPROFILE%\go\bin;%DOCK_UNDEFINED_VAR%\bin;C:\dock\bin') -and ($key.GetValueKind('Path') -eq 'ExpandString'))
    Expect 'user PATH update is idempotent' ($first -and -not $second -and -not $third)
    $key.DeleteValue('Path')
    Expect 'user PATH created when missing' ((Add-UserPath $key 'C:\dock\bin') -and ($key.GetValue('Path') -ceq 'C:\dock\bin'))
  } finally {
    $key.Close()
    [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree($scratch, $false)
  }
}

Stop-Job $server -ErrorAction SilentlyContinue
Remove-Job $server -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
exit $script:fail
