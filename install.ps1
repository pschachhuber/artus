# Installs Artus on Windows. In PowerShell:
#
#   irm https://raw.githubusercontent.com/pschachhuber/artus/main/install.ps1 | iex
#
# Downloads the program for this computer from the latest GitHub Release,
# checks it against the release's checksums.txt and puts it in
# %LOCALAPPDATA%\Programs\Artus, a folder of your own, so that Artus can update
# itself there. Adds that folder to your PATH. Run it again to reinstall.
#
#   $env:ARTUS_INSTALL_DIR  install somewhere else
#   $env:ARTUS_VERSION      install a given release, such as v0.1.0
#
# A file fetched this way is not marked as downloaded from the internet, so
# Windows starts it without the SmartScreen prompt it shows for an unsigned
# file saved by a browser.

# Everything is in one block, so that a download cut off half way runs nothing.
& {
    $ErrorActionPreference = 'Stop'
    # The progress bar slows downloads to a crawl in Windows PowerShell.
    $ProgressPreference = 'SilentlyContinue'
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'pschachhuber/artus'
    $dir = if ($env:ARTUS_INSTALL_DIR) { $env:ARTUS_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\Artus' }
    $version = if ($env:ARTUS_VERSION) { $env:ARTUS_VERSION } else { 'latest' }

    # The system's architecture, not this PowerShell's, which may be emulated.
    $machine = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $machine = $env:PROCESSOR_ARCHITEW6432 }
    try { $machine = [string][System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture } catch {}
    $arch = switch ($machine) {
        'X64' { 'amd64' }
        'AMD64' { 'amd64' }
        'Arm64' { 'arm64' }
        default { throw "artus: no build for $machine" }
    }
    $asset = "artus_windows_$arch.exe"
    if ($version -eq 'latest') {
        $base = "https://github.com/$repo/releases/latest/download"
    } else {
        $base = "https://github.com/$repo/releases/download/$version"
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading $asset ($version)"
        $file = Join-Path $tmp $asset
        $sums = Join-Path $tmp 'checksums.txt'
        foreach ($get in @(@("$base/$asset", $file), @("$base/checksums.txt", $sums))) {
            try {
                Invoke-WebRequest -UseBasicParsing -Uri $get[0] -OutFile $get[1]
            } catch {
                throw "artus: cannot download $($get[0]): $($_.Exception.Message)"
            }
        }
        $want = Get-Content $sums | ForEach-Object {
            $f = -split $_
            if ($f.Count -eq 2 -and $f[1] -eq $asset) { $f[0] }
        } | Select-Object -First 1
        if (-not $want) { throw "artus: checksums.txt lists no $asset" }
        $got = (Get-FileHash -Algorithm SHA256 $file).Hash.ToLowerInvariant()
        if ($got -ne $want) { throw "artus: $asset does not match its checksum (got $got, want $want); nothing was installed" }

        # A running Artus cannot be overwritten, but it can be renamed. Artus
        # removes the .old file when it next starts, as after an update.
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $exe = Join-Path $dir 'artus.exe'
        $new = Join-Path $dir '.artus-install.exe'
        Copy-Item -Force $file $new
        if (Test-Path $exe) {
            if (Test-Path "$exe.old") { Remove-Item -Force "$exe.old" }
            Move-Item $exe "$exe.old"
        }
        Move-Item $new $exe
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }

    $installed = & $exe version
    if ($LASTEXITCODE -ne 0) { throw "artus: $exe was installed but does not run here" }
    Write-Host "Installed $installed to $exe"

    # Add the folder to the user's PATH as stored in the registry, keeping
    # entries such as %USERPROFILE%\... unexpanded.
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    try {
        $path = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $entries = $path -split ';' | ForEach-Object { [Environment]::ExpandEnvironmentVariables($_).TrimEnd('\') }
        $added = $entries -notcontains $dir.TrimEnd('\')
        if ($added) {
            $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
            if ($key.GetValueNames() -contains 'Path') { $kind = $key.GetValueKind('Path') }
            $key.SetValue('Path', ((@($path.TrimEnd(';'), $dir) | Where-Object { $_ }) -join ';'), $kind)
        }
    } finally {
        $key.Close()
    }
    if ($added) {
        # Setting any user variable tells Explorer, and the terminals it
        # starts from now on, to read the environment again.
        [Environment]::SetEnvironmentVariable('ARTUS_INSTALLED', '1', 'User')
        [Environment]::SetEnvironmentVariable('ARTUS_INSTALLED', $null, 'User')
        Write-Host "Added $dir to your PATH"
    }
    if ((($env:Path -split ';') | ForEach-Object { $_.TrimEnd('\') }) -notcontains $dir.TrimEnd('\')) {
        $env:Path = "$env:Path;$dir"
    }
    Write-Host 'Start it with: artus'
}
