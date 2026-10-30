# Native Go service installation. Run from an elevated PowerShell session.
[CmdletBinding()]
param(
    [ValidateSet('master','agent')][string]$Role,
    [ValidateSet('auto','github','jdoss')][string]$Source = 'auto',
    [string]$Version = 'latest',
    [string]$ReleaseUrl,
    [string]$PackageDir,
    [string]$AdoptConfig,
    [string]$CliPath,
    [switch]$ReplaceCli,
    [string]$InstallDir,
    [string]$ConfigDir,
    [string]$DataDir,
    [string]$BackupDir,
    [string]$ServiceName,
    [string]$Listen = '0.0.0.0:1685',
    [string]$MasterUrl,
    [switch]$Upgrade,
    [switch]$Yes,
    [ValidateRange(10,600)][int]$StartupTimeout = 120
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# Source resolution is kept separate from service operations so malformed downloads
# are rejected before an existing installation is stopped.
function Protect-Directory([string]$Path) {
    [IO.Directory]::CreateDirectory($Path) | Out-Null
    $acl = New-Object Security.AccessControl.DirectorySecurity
    $acl.SetAccessRuleProtection($true,$false)
    foreach ($sid in @('S-1-5-18','S-1-5-32-544',$identity.User.Value)) {
        $principal = New-Object Security.Principal.SecurityIdentifier($sid)
        $rule = New-Object Security.AccessControl.FileSystemAccessRule($principal,'FullControl','ContainerInherit,ObjectInherit','None','Allow')
        $acl.AddAccessRule($rule)
    }
    Set-Acl -LiteralPath $Path -AclObject $acl
}
function Normalize-ReleaseVersion([string]$Value) {
    if ($Value -ceq 'latest') { return $Value }
    if ($Value -cnotmatch '^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?\z') { throw 'Version must be latest or a release such as v4.1.5.' }
    return 'v' + ($Value -replace '^v','')
}
function Normalize-ReleaseUrl([string]$Value) {
    $url = $Value.TrimEnd('/')
    $legacy = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer'
    if ($url -ceq $legacy -or $url.StartsWith($legacy + '/', [StringComparison]::Ordinal)) {
        return 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer' + $url.Substring($legacy.Length)
    }
    return $url
}
function Invoke-SourceRequest([string]$Url,[int]$Timeout = 20,[string]$Target) {
    $arguments = @{Uri=$Url; UseBasicParsing=$true; TimeoutSec=$Timeout; Headers=@{'User-Agent'='oss-transfer-installer'; Accept='application/vnd.github+json'}}
    if ($Target) { $arguments.OutFile=$Target; $arguments.PassThru=$true }
    try { $response = Invoke-WebRequest @arguments }
    catch { throw [Net.WebException]::new('Could not download release metadata or archive.', $_.Exception) }
    if ($response.BaseResponse.PSObject.Properties['ResponseUri']) { $finalUri = $response.BaseResponse.ResponseUri } else { $finalUri = $response.BaseResponse.RequestMessage.RequestUri }
    if ($finalUri.Scheme -cne 'https') { throw 'Download redirected to an insecure URL.' }
    if (-not $Target) { return [string]$response.Content }
}
function Get-PublicIPCountry {
    try {
        $trace = Invoke-SourceRequest 'https://www.cloudflare.com/cdn-cgi/trace' 3
        $locations = @($trace -split "\r?\n" | Where-Object { $_ -cmatch '^loc=[A-Z]{2}$' })
        if ($locations.Count -eq 1 -and $locations[0] -cne 'loc=XX') { return $locations[0].Substring(4) }
    } catch { }
    try {
        $country = (Invoke-SourceRequest 'https://ipinfo.io/country' 3).Trim()
        if ($country -cmatch '^[A-Z]{2}$' -and $country -cne 'XX') { return $country }
    } catch { }
    return ''
}
function Get-SourceMetadata([string]$Selected,[string]$RequestedVersion,[string]$Architecture) {
    if ($Selected -eq 'jdoss') {
        $base = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer'
        $url = "$base/$RequestedVersion"
        $manifest = Invoke-SourceRequest "$url/manifest.tsv"
        if ([Text.Encoding]::UTF8.GetByteCount($manifest) -ge 65536) { throw 'Manifest too large.' }
        $rows = @($manifest -split "\r?\n")
        if (@($rows | Where-Object { $_ -ceq "format`toss-transfer-go-v1" }).Count -ne 1) { throw 'Unknown release format.' }
        $versions = @($rows | Where-Object { $_.StartsWith("version`t") })
        $commits = @($rows | Where-Object { $_.StartsWith("commit`t") })
        if ($versions.Count -ne 1 -or $versions[0] -cnotmatch '^version\t([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)$') { throw 'Invalid release version.' }
        $tag = 'v' + $Matches[1]
        if ($commits.Count -ne 1 -or $commits[0] -cnotmatch '^commit\t[a-f0-9]{40}$') { throw 'Invalid release commit.' }
        if ($RequestedVersion -ne 'latest' -and $tag -cne $RequestedVersion) { throw 'Requested and published release versions differ.' }
        if ($RequestedVersion -eq 'latest') {
            $url = "$base/$tag"
            # Once metadata identifies a version, a partial publication is fatal.
            try { $pinned = Invoke-SourceRequest "$url/manifest.tsv" }
            catch { throw 'Could not verify the versioned release manifest.' }
            if ($manifest -cne $pinned) { throw 'Latest and versioned release manifests differ; retry after publication finishes.' }
        }
        return @{Source='jdoss'; Version=$tag; ReleaseUrl=$url}
    }
    $endpoint = if ($RequestedVersion -eq 'latest') { 'latest' } else { 'tags/' + $RequestedVersion }
    $body = Invoke-SourceRequest "https://api.github.com/repos/MoonWX/oss-transfer/releases/$endpoint"
    if ([Text.Encoding]::UTF8.GetByteCount($body) -gt 1048576) { throw 'Release metadata too large.' }
    $release = $body | ConvertFrom-Json
    if ($release.tag_name -isnot [string] -or $release.tag_name -cnotmatch '^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?\z' -or $release.draft -isnot [bool] -or $release.draft -ne $false -or $release.assets -isnot [Array]) { throw 'Invalid GitHub release identity.' }
    if ($RequestedVersion -ne 'latest' -and $release.tag_name -cne $RequestedVersion) { throw 'Requested and published release versions differ.' }
    $name = "oss-transfer-windows-$Architecture.zip"
    $assets = @($release.assets | Where-Object { $_.name -ceq $name })
    if ($assets.Count -ne 1) { throw 'Missing or duplicate GitHub platform archive.' }
    $asset = $assets[0]
    $expectedURL = "https://github.com/MoonWX/oss-transfer/releases/download/$($release.tag_name)/$name"
    if ($asset.browser_download_url -cne $expectedURL -or $asset.digest -cnotmatch '^sha256:[a-f0-9]{64}$' -or [string]$asset.size -cnotmatch '^[1-9][0-9]{0,9}$' -or [long]$asset.size -gt 2147483647) { throw 'Invalid GitHub archive URL, checksum or size.' }
    return @{Source='github'; Version=[string]$release.tag_name; Name=$name; Url=$expectedURL; Hash=$asset.digest.Substring(7); Size=[long]$asset.size; Architecture=$Architecture}
}
function Resolve-DownloadSource([string]$Preference,[string]$RequestedVersion,[string]$Architecture) {
    $selected = $Preference
    if ($selected -eq 'auto') { $selected = if ((Get-PublicIPCountry) -ceq 'CN') { 'jdoss' } else { 'github' } }
    try { return Get-SourceMetadata $selected $RequestedVersion $Architecture }
    catch [Net.WebException] {
        if ($Preference -ne 'auto') { throw }
        $alternate = if ($selected -eq 'github') { 'jdoss' } else { 'github' }
        Write-Warning "Could not reach $selected; trying $alternate."
        return Get-SourceMetadata $alternate $RequestedVersion $Architecture
    }
}
function Get-RegularZipEntry($Archive,[string]$Name,[long]$MaximumSize) {
    $entries = @($Archive.Entries | Where-Object { $_.FullName -ceq $Name })
    if ($entries.Count -ne 1) { throw "Archive member must occur once: $Name" }
    $entry = $entries[0]
    $kind = ($entry.ExternalAttributes -shr 16) -band 0xF000
    if ($kind -notin @(0,0x8000) -or ($entry.ExternalAttributes -band 0x10) -ne 0 -or $entry.Length -le 0 -or $entry.Length -gt $MaximumSize) { throw "Invalid regular archive member: $Name" }
    return $entry
}
function Read-ZipEntryText($Entry) {
    $stream = $Entry.Open()
    try {
        $reader = [IO.StreamReader]::new($stream,[Text.UTF8Encoding]::new($false,$true))
        try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
    } finally { $stream.Dispose() }
}
function Get-ZipEntryHash($Entry) {
    $stream = $Entry.Open()
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash($stream))).Replace('-','').ToLowerInvariant() }
    finally { $sha.Dispose(); $stream.Dispose() }
}
function New-GitHubPackage($Release,[string]$Directory) {
    [IO.Directory]::CreateDirectory($Directory) | Out-Null
    $archivePath = Join-Path $Directory $Release.Name
    Invoke-SourceRequest $Release.Url 600 $archivePath
    if ((Get-Item -LiteralPath $archivePath).Length -ne $Release.Size -or (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant() -cne $Release.Hash) { throw 'Archive checksum mismatch.' }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [IO.Compression.ZipFile]::OpenRead($archivePath)
    try {
        $root = "oss-transfer-windows-$($Release.Architecture)/"
        $versionEntry = Get-RegularZipEntry $zip ($root+'VERSION') 65535
        $sumEntry = Get-RegularZipEntry $zip ($root+'SHA256SUMS') 65535
        $checksums = @{}
        foreach ($line in ((Read-ZipEntryText $sumEntry) -split "\r?\n")) {
            if (-not $line) { continue }
            if ($line -cnotmatch '^([a-f0-9]{64}) [ *](VERSION|oss-transfer-(?:master|agent|cli)\.exe)$') { throw 'Invalid package checksum entry.' }
            if ($checksums.ContainsKey($Matches[2])) { throw 'Duplicate package checksum entry.' }
            $checksums[$Matches[2]] = $Matches[1]
        }
        if ($checksums.Count -ne 4 -or (Get-ZipEntryHash $versionEntry) -cne $checksums['VERSION']) { throw 'Package VERSION checksum mismatch.' }
        $fields = @{}
        foreach ($line in ((Read-ZipEntryText $versionEntry) -split "\r?\n")) {
            if (-not $line) { continue }
            if ($line -cnotmatch '^([A-Za-z]+): (.+)$' -or $fields.ContainsKey($Matches[1])) { throw 'Invalid package VERSION entry.' }
            $fields[$Matches[1]] = $Matches[2]
        }
        if ($fields['Version'] -cne $Release.Version.Substring(1) -or $fields['Platform'] -cne "windows/$($Release.Architecture)" -or $fields['FullCommit'] -cnotmatch '^[a-f0-9]{40}$' -or $fields['Commit'] -cnotmatch '^[a-f0-9]{7,40}$' -or -not $fields['FullCommit'].StartsWith($fields['Commit'],[StringComparison]::Ordinal)) { throw 'Package version, commit or platform differs from the release.' }
        $manifest = [Collections.Generic.List[string]]::new()
        $manifest.Add("format`toss-transfer-go-v1")
        $manifest.Add("version`t$($fields['Version'])")
        $manifest.Add("commit`t$($fields['FullCommit'])")
        foreach ($program in @('master','agent','cli')) {
            $member = "oss-transfer-$program.exe"
            $entry = Get-RegularZipEntry $zip ($root+$member) 2147483647
            if (-not $checksums.ContainsKey($member) -or (Get-ZipEntryHash $entry) -cne $checksums[$member]) { throw 'Package binary checksum mismatch.' }
            $manifest.Add("file`twindows`t$($Release.Architecture)`t$program`toss-transfer-$program-windows-$($Release.Architecture).exe`t$($checksums[$member])`t$($entry.Length)")
        }
        $manifest.Add("archive`twindows`t$($Release.Architecture)`t$($Release.Name)`t$($Release.Hash)`t$($Release.Size)")
        [IO.File]::WriteAllText((Join-Path $Directory 'manifest.tsv'),(($manifest -join "`n")+"`n"),[Text.UTF8Encoding]::new($false))
    } finally { $zip.Dispose() }
}

$Version = Normalize-ReleaseVersion $Version
$sourceWork = $null
try {
if ($env:OS -ne 'Windows_NT') { throw 'This installer requires Windows.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run PowerShell as Administrator.' }
if ($ReleaseUrl -and $PackageDir) { throw 'Choose -ReleaseUrl or -PackageDir.' }
if ($ReleaseUrl) { $ReleaseUrl = Normalize-ReleaseUrl $ReleaseUrl }
if ($AdoptConfig -and $Upgrade) { throw 'Choose -AdoptConfig or -Upgrade.' }
$architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
$arch = switch ($architecture) { 'x64' {'amd64'} 'arm64' {'arm64'} default {throw 'Unsupported CPU architecture.'} }
if (-not $ReleaseUrl -and -not $PackageDir) {
    $resolved = Resolve-DownloadSource $Source $Version $arch
    Write-Host "Download source: $($resolved.Source) ($($resolved.Version))"
    if ($resolved.Source -eq 'github') {
        $sourceWork = Join-Path ([IO.Path]::GetTempPath()) ('oss-transfer-source-'+[Guid]::NewGuid().ToString('N'))
        Protect-Directory $sourceWork
        New-GitHubPackage $resolved $sourceWork
        $PackageDir = $sourceWork
    } else { $ReleaseUrl = $resolved.ReleaseUrl }
}

if ($Upgrade) {
    if (-not $InstallDir) {
        $candidates = @{}
        function Add-Installation([string]$Directory) {
            $recordPath = Join-Path $Directory 'install.json'
            if (-not (Test-Path -LiteralPath $recordPath -PathType Leaf)) { return }
            if ((Get-Item -LiteralPath $recordPath -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { return }
            $record = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json
            if ($record.role -notin @('master','agent') -or ($Role -and $record.role -ne $Role) -or ($ServiceName -and $record.serviceName -ne $ServiceName)) { return }
            $candidates[$Directory] = $record
        }
        foreach ($installedRole in @('master','agent')) {
            Add-Installation (Join-Path $env:ProgramFiles "OSS Transfer\$installedRole")
        }
        foreach ($installedService in @(Get-CimInstance Win32_Service)) {
            if ($installedService.PathName -match '^"([^"]+\\oss-transfer-(?:master|agent)\.exe)"(?:\s|$)') {
                Add-Installation (Split-Path $Matches[1] -Parent)
            }
        }
        if ($candidates.Count -eq 0) { throw 'No managed installation found; use -InstallDir for an installation without a service definition.' }
        Write-Host 'Upgrade installations:'
        foreach ($directory in @($candidates.Keys | Sort-Object)) { Write-Host "  $($candidates[$directory].serviceName): $directory" }
        if (-not $Yes -and (Read-Host 'Back up and upgrade these installations? [y/N]') -notmatch '^[yY]$') { return }
        # Resolve latest before dispatching either role, then pass that fixed URL.
        if ($ReleaseUrl -and $ReleaseUrl.EndsWith('/latest',[StringComparison]::Ordinal)) {
            $uri = [Uri]$ReleaseUrl
            if (-not $uri.IsAbsoluteUri -or $uri.UserInfo -or $uri.Query -or $uri.Fragment -or ($uri.Scheme -ne 'https' -and -not ($uri.Scheme -eq 'http' -and $uri.IsLoopback)) -or $uri.AbsolutePath -notmatch '^/[A-Za-z0-9._~/-]+$') { throw 'Release URL must use HTTPS.' }
            $latestManifest = [IO.Path]::GetTempFileName()
            $versionManifest = [IO.Path]::GetTempFileName()
            try {
                function Fetch-VersionManifest([string]$Url,[string]$Target) {
                    $response = Invoke-WebRequest -UseBasicParsing -Uri "$Url/manifest.tsv" -OutFile $Target -PassThru -TimeoutSec 600
                    if ($response.BaseResponse.PSObject.Properties['ResponseUri']) { $finalUri = $response.BaseResponse.ResponseUri } else { $finalUri = $response.BaseResponse.RequestMessage.RequestUri }
                    if ($finalUri.Scheme -ne 'https' -and -not ($finalUri.Scheme -eq 'http' -and $finalUri.IsLoopback)) { throw 'Release manifest redirected to an insecure URL.' }
                    if ((Get-Item -LiteralPath $Target).Length -ge 65536) { throw 'Manifest too large.' }
                }
                Fetch-VersionManifest $ReleaseUrl $latestManifest
                $rows = @(Get-Content -LiteralPath $latestManifest)
                if (@($rows | Where-Object { $_ -eq "format`toss-transfer-go-v1" }).Count -ne 1) { throw 'Unknown release format.' }
                $versions = @($rows | Where-Object { $_.StartsWith("version`t") })
                if ($versions.Count -ne 1) { throw 'Invalid version entry.' }
                $pinnedVersion = ($versions[0] -split "`t")[1]
                if ($pinnedVersion -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') { throw 'Invalid latest release version.' }
                $ReleaseUrl = $ReleaseUrl.Substring(0,$ReleaseUrl.Length-7) + '/v' + $pinnedVersion
                Fetch-VersionManifest $ReleaseUrl $versionManifest
                if ((Get-FileHash -LiteralPath $latestManifest -Algorithm SHA256).Hash -cne (Get-FileHash -LiteralPath $versionManifest -Algorithm SHA256).Hash) { throw 'Latest and versioned release manifests differ; retry after publication finishes.' }
            } finally {
                Remove-Item -LiteralPath $latestManifest,$versionManifest -Force -ErrorAction SilentlyContinue
            }
        }
        foreach ($directory in @($candidates.Keys | Sort-Object)) {
            $childArguments = @{}
            foreach ($key in $PSBoundParameters.Keys) { $childArguments[$key] = $PSBoundParameters[$key] }
            if ($ReleaseUrl) { $childArguments.ReleaseUrl = $ReleaseUrl }
            if ($PackageDir) { $childArguments.PackageDir = $PackageDir; $childArguments.Remove('ReleaseUrl') }
            $childArguments.InstallDir = $directory
            $childArguments.Yes = $true
            & $PSCommandPath @childArguments
        }
        return
    }
    $recordPath = Join-Path $InstallDir 'install.json'
    if (-not (Test-Path -LiteralPath $recordPath -PathType Leaf) -or ((Get-Item -LiteralPath $recordPath -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'No managed installation found in the selected directory.' }
    $record = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json
    if ($record.role -notin @('master','agent') -or ($Role -and $Role -ne $record.role)) { throw 'Installed role differs from -Role.' }
    $Role = $record.role
    if (-not $ServiceName) { $ServiceName = $record.serviceName }
    if (-not $ConfigDir) { $ConfigDir = Split-Path $record.config -Parent }
    if (-not $CliPath -and $record.PSObject.Properties['cliPath']) { $CliPath = $record.cliPath }
    if (-not $DataDir) {
        if (-not (Test-Path -LiteralPath $record.config -PathType Leaf) -or ((Get-Item -LiteralPath $record.config -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Installed configuration is missing or linked.' }
        $savedConfig = Get-Content -LiteralPath $record.config -Raw | ConvertFrom-Json
        $DataDir = $savedConfig.dataDirectory
        if (-not $DataDir) { throw 'Installed data directory is missing.' }
    }
}
if (-not $Role) { $Role = 'master' }
if (-not $ServiceName) { $ServiceName = "OSSTransfer$((Get-Culture).TextInfo.ToTitleCase($Role))" }
if ($ServiceName -notmatch '^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$') { throw 'Invalid service name.' }
$base = Join-Path $env:ProgramData "OSS Transfer\$Role"
if (-not $InstallDir) { $InstallDir = Join-Path $env:ProgramFiles "OSS Transfer\$Role" }
if (-not $ConfigDir) { $ConfigDir = Join-Path $base 'config' }
if (-not $DataDir) { $DataDir = Join-Path $base 'data' }
function Resolve-BackupDirectory([string]$Requested,[string]$RoleName) {
    if ($Requested) { return $Requested }
    return Join-Path (Join-Path $env:ProgramData "OSS Transfer\$RoleName") 'backups'
}
$BackupDir = Resolve-BackupDirectory $BackupDir $Role
function Assert-DirectoryPath([string]$Path) {
    if (-not [IO.Path]::IsPathRooted($Path) -or $Path.StartsWith('\\') -or $Path -match '[\x00-\x1f"%]' -or [IO.Path]::GetFullPath($Path).TrimEnd('\') -cne $Path -or $Path -match '^[A-Za-z]:$') { throw "Use a clean local absolute path: $Path" }
    $ancestor = $Path
    while ($ancestor) {
        if (Test-Path -LiteralPath $ancestor) {
            $item = Get-Item -LiteralPath $ancestor -Force
            if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Directory is a link or non-directory: $ancestor" }
        }
        $ancestor = Split-Path $ancestor -Parent
    }
}
$directories = @($InstallDir,$ConfigDir,$DataDir,$BackupDir)
foreach ($path in $directories) { Assert-DirectoryPath $path }
for ($i=0; $i -lt 4; $i++) { for ($j=0; $j -lt 4; $j++) {
    if ($i -ne $j -and ($directories[$i].Equals($directories[$j],[StringComparison]::OrdinalIgnoreCase) -or $directories[$i].StartsWith($directories[$j]+'\',[StringComparison]::OrdinalIgnoreCase))) { throw 'Binary, config, data and backup directories must be separate.' }
} }
function Assert-RegularFile([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf) -or ((Get-Item -LiteralPath $Path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Expected a regular file: $Path" }
}
function Protect-File([string]$Path) {
    Assert-RegularFile $Path
    $acl = New-Object Security.AccessControl.FileSecurity
    $acl.SetAccessRuleProtection($true,$false)
    foreach ($sid in @('S-1-5-18','S-1-5-32-544',$identity.User.Value)) {
        $principal = New-Object Security.Principal.SecurityIdentifier($sid)
        $rule = New-Object Security.AccessControl.FileSystemAccessRule($principal,'FullControl','Allow')
        $acl.AddAccessRule($rule)
    }
    Set-Acl -LiteralPath $Path -AclObject $acl
}
function Write-UTF8([string]$Path,[string]$Text) { [IO.File]::WriteAllText($Path,$Text,(New-Object Text.UTF8Encoding($false))) }
$config = Join-Path $ConfigDir 'service.json'
$binary = Join-Path $InstallDir "oss-transfer-$Role.exe"
$metadata = Join-Path $InstallDir 'install.json'
if (-not $CliPath) { $CliPath = Join-Path $InstallDir 'oss-transfer-cli.exe' }
Assert-DirectoryPath (Split-Path $CliPath -Parent)
if ([IO.Path]::GetFileName($CliPath) -cne 'oss-transfer-cli.exe') { throw 'CLI path must end in oss-transfer-cli.exe.' }
foreach ($path in @($ConfigDir,$DataDir,$BackupDir)) {
    if ($CliPath.StartsWith($path+'\',[StringComparison]::OrdinalIgnoreCase)) { throw 'CLI must be outside configuration, data and backup directories.' }
}
$hadCli = Test-Path -LiteralPath $CliPath
if ($hadCli) { Assert-RegularFile $CliPath }
$hadCliSidecars = $false
foreach ($suffix in @('cluster.json','local-auth.json')) {
    $sidecar = $CliPath+'.'+$suffix
    if (Get-Item -LiteralPath $sidecar -Force -ErrorAction SilentlyContinue) {
        Assert-RegularFile $sidecar
        $hadCliSidecars = $true
    }
}
$hadDataCLIConnections = $false
foreach ($name in @('cli-cluster.json','cli-local-auth.json')) {
    $connection = Join-Path $DataDir $name
    if (Get-Item -LiteralPath $connection -Force -ErrorAction SilentlyContinue) {
        Assert-RegularFile $connection
        $hadDataCLIConnections = $true
    }
}
$cliChanged = $false
$database = Join-Path $DataDir "$Role.db"
$existing = Test-Path -LiteralPath $metadata
$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existing) {
    if (-not $Upgrade) { throw 'Installation exists; rerun with -Upgrade.' }
    Assert-RegularFile $metadata; Assert-RegularFile $binary; Assert-RegularFile $config
    $installed = Get-Content -LiteralPath $metadata -Raw | ConvertFrom-Json
    if ($installed.role -ne $Role -or $installed.serviceName -ne $ServiceName -or $installed.config -ne $config) { throw 'Existing installation identity does not match.' }
} else {
    if ($Upgrade) { throw 'No managed installation found; V4.0.x requires database migration first.' }
    if ($service) { throw 'An unmanaged service already exists.' }
    foreach ($path in @($InstallDir,$ConfigDir,$DataDir)) {
        if ($AdoptConfig -and $path -eq $DataDir) { continue }
        if ((Test-Path -LiteralPath $path) -and @(Get-ChildItem -LiteralPath $path -Force).Count -gt 0) { throw "Refusing nonempty directory: $path" }
    }
}
if ($existing -and $installed.PSObject.Properties['cliPath'] -and $installed.cliPath -ne $CliPath) { throw 'CLI path differs from the installed path; keep -CliPath unchanged during upgrade.' }
if (($hadCli -or $hadCliSidecars) -and (-not $existing -or -not $installed.PSObject.Properties['cliPath'] -or $installed.cliPath -ne $CliPath) -and -not $ReplaceCli) { throw "CLI already exists: $CliPath; use -ReplaceCli to back it up and replace it, or choose -CliPath." }
$work = Join-Path ([IO.Path]::GetTempPath()) ('oss-transfer-install-'+[Guid]::NewGuid().ToString('N'))
Protect-Directory $work
$changed = $false
$installationSucceeded = $false
$agentMaintenanceStarted = $false
$backup = $null
$script:ownedBackup = $null
$script:startupCommitted = $false
$hadDatabaseBackup = $false
$wasRunning = $false
$oldService = $null
function Fetch([string]$Name) {
    $target = Join-Path $work $Name
    if ($PackageDir) { $source=Join-Path $PackageDir $Name; Assert-RegularFile $source; Copy-Item -LiteralPath $source -Destination $target }
    else {
        $response = Invoke-WebRequest -UseBasicParsing -Uri "$ReleaseUrl/$Name" -OutFile $target -PassThru -TimeoutSec 600
        if ($response.BaseResponse.PSObject.Properties['ResponseUri']) { $finalUri = $response.BaseResponse.ResponseUri } else { $finalUri = $response.BaseResponse.RequestMessage.RequestUri }
        if ($finalUri.Scheme -ne 'https' -and -not ($finalUri.Scheme -eq 'http' -and $finalUri.IsLoopback)) { throw 'Release download redirected to an insecure URL.' }
    }
    return $target
}
function Invoke-Native([string]$Executable,[string[]]$Arguments) {
    # Windows PowerShell 5.1 treats native stderr as an ErrorRecord. Progress
    # messages must not abort an upgrade; the process exit code decides success.
    $savedPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        # Native commands update the global automatic variable. A local value
        # would shadow it in Windows PowerShell 5.1.
        $global:LASTEXITCODE = $null
        $output = & $Executable @Arguments
        $exitCode = $global:LASTEXITCODE
    } finally { $ErrorActionPreference = $savedPreference }
    if ($null -eq $exitCode -or $exitCode -ne 0) { throw "Native command failed (exit $exitCode)." }
    return $output
}
function Service-Record { Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue }
function Maintain-Backups([string]$Action) {
    $script:backupMaintenanceSucceeded = $false
    try {
        $arguments = @('--backup-maintenance',$Action,'--backup-dir',$BackupDir,'--config',$config)
        if ($Action -eq 'complete') { $arguments += @('--backup-entry',[IO.Path]::GetFileName($backup)) }
        $result = (Invoke-Native $candidate $arguments | Out-String) | ConvertFrom-Json
        if ($Action -eq 'prune' -and $result.count -gt 0) {
            Write-Host ("Removed {0} completed temporary backup(s)." -f $result.count)
        }
        $script:backupMaintenanceSucceeded = $true
    } catch {
        Write-Warning ("Could not finish upgrade backup maintenance ({0}). Check {1}. {2}" -f $Action,$BackupDir,$_.Exception.Message) -WarningAction Continue
    }
}
function Assert-BackupSpace([long]$Required,[long]$Available) {
    if ($Available -lt $Required) {
        throw ("Not enough space for the upgrade backup: {0} needs {1} MiB; {2} MiB available. Free space or choose -BackupDir on another disk. The service has not been stopped." -f $BackupDir,[Math]::Ceiling($Required/1MB),[Math]::Floor($Available/1MB))
    }
}
function Test-BackupSpace {
    $required = [long](64MB)
    foreach ($path in @($database,($database+'.key'),$binary,$CliPath,($CliPath+'.cluster.json'),($CliPath+'.local-auth.json'),(Join-Path $DataDir 'cli-cluster.json'),(Join-Path $DataDir 'cli-local-auth.json'))) {
        if (Test-Path -LiteralPath $path -PathType Leaf) { $required += (Get-Item -LiteralPath $path).Length }
    }
    $driveRoot = [IO.Path]::GetPathRoot($BackupDir)
    $drive = [IO.DriveInfo]::new($driveRoot)
    $available = $drive.AvailableFreeSpace
    Assert-BackupSpace $required $available
}
function Prepare-BackupDirectory {
    Assert-DirectoryPath $BackupDir
    Protect-Directory $BackupDir
    $probe = Join-Path $BackupDir ('.write-test-'+[Guid]::NewGuid().ToString('N'))
    $stream = $null
    $created = $false
    try {
        $stream = [IO.File]::Open($probe,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
        $created = $true
        $stream.WriteByte(0)
        $stream.Flush($true)
    } catch {
        throw ("Cannot write the temporary backup directory: {0}. The service has not been stopped. {1}" -f $BackupDir,$_.Exception.GetBaseException().Message)
    } finally {
        if ($stream) { $stream.Dispose() }
        if ($created) { Remove-Item -LiteralPath $probe -Force }
    }
}
function New-TemporaryBackup {
    $path = Join-Path $BackupDir ($ServiceName+'-'+[DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')+'-'+[Guid]::NewGuid().ToString('N'))
    if (Test-Path -LiteralPath $path) { throw 'Temporary backup directory already exists.' }
    Protect-Directory $path
    $script:ownedBackup = $path
    Write-Host "Temporary backup: $path"
    return $path
}
function Remove-TemporaryBackup {
    try {
        if (-not $backup -or $backup -cne $script:ownedBackup -or -not (Split-Path $backup -Parent).Equals($BackupDir,[StringComparison]::OrdinalIgnoreCase)) {
            throw 'Temporary backup does not belong to this installation attempt.'
        }
        Assert-DirectoryPath $backup
        # First-time CLI replacement and adoption do not have complete managed
        # installation metadata. Only remove their exact, newly created files.
        foreach ($file in @(Get-ChildItem -LiteralPath $backup -Force)) {
            if ($file.Name -notin @('database','database.key','service.json','cli.exe','cli.cluster.json','cli.local-auth.json','data-cli.cluster.json','data-cli.local-auth.json')) { throw 'Unexpected temporary backup contents.' }
            Assert-RegularFile $file.FullName
        }
        Remove-Item -LiteralPath $backup -Recurse -Force
        Write-Host "Removed temporary backup: $backup"
    } catch {
        Write-Warning ("Could not remove the temporary backup. Retained at {0}. {1}" -f $backup,$_.Exception.GetBaseException().Message) -WarningAction Continue
    }
}
function Finish-TemporaryBackup {
    if (-not $installationSucceeded) { return }
    Commit-Start
    if (-not $script:startupCommitted) {
        if ($backup) { Write-Warning "Startup confirmation was not saved. Backup retained at $backup" -WarningAction Continue }
        return
    }
    if (-not $backup) { return }
    if ($existing) {
        Maintain-Backups 'complete'
        if ($script:backupMaintenanceSucceeded) { Maintain-Backups 'prune' }
        if (Test-Path -LiteralPath $backup) { Write-Warning "Temporary backup retained at $backup" -WarningAction Continue }
    } else { Remove-TemporaryBackup }
}
function Copy-CLIConnection([string]$Source,[string]$Destination,[bool]$Private) {
    Assert-RegularFile $Source
    if (Get-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue) { Assert-RegularFile $Destination }
    $temporary = $Destination+'.restore-'+[Guid]::NewGuid().ToString('N')
    try {
        $empty = [IO.File]::Open($temporary,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
        $empty.Dispose()
        # Protect the empty file before copying any private credential bytes.
        if ($Private) { Protect-File $temporary }
        [IO.File]::WriteAllBytes($temporary,[IO.File]::ReadAllBytes($Source))
        Move-Item -LiteralPath $temporary -Destination $Destination -Force
    } finally {
        if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
    }
}
function Backup-CLIConnections {
    foreach ($suffix in @('cluster.json','local-auth.json')) {
        foreach ($kind in @('cli','data-cli')) {
            $source = if ($kind -eq 'cli') { $CliPath+'.'+$suffix } else { Join-Path $DataDir ('cli-'+$suffix) }
            if (Get-Item -LiteralPath $source -Force -ErrorAction SilentlyContinue) {
                Copy-CLIConnection $source (Join-Path $backup ($kind+'.'+$suffix)) $true
            }
        }
    }
}
function Restore-CLIConnections {
    foreach ($suffix in @('cluster.json','local-auth.json')) {
        foreach ($kind in @('cli','data-cli')) {
            $destination = if ($kind -eq 'cli') { $CliPath+'.'+$suffix } else { Join-Path $DataDir ('cli-'+$suffix) }
            if (Get-Item -LiteralPath $destination -Force -ErrorAction SilentlyContinue) { Assert-RegularFile $destination }
            $saved = if ($backup) { Join-Path $backup ($kind+'.'+$suffix) } else { $null }
            if ($saved -and (Get-Item -LiteralPath $saved -Force -ErrorAction SilentlyContinue)) {
                Copy-CLIConnection $saved $destination ($suffix -eq 'local-auth.json')
            } elseif (Test-Path -LiteralPath $destination) {
                Remove-Item -LiteralPath $destination -Force
            }
        }
    }
}
function Stop-OwnedService {
    $current = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($current -and $current.Status -ne 'Stopped') { Stop-Service -Name $ServiceName; $current.WaitForStatus('Stopped',[TimeSpan]::FromSeconds(50)) }
}
function Prepare-Start {
    $script:startupLogLines = 0
    if (Test-Path -LiteralPath $script:logFile -PathType Leaf) {
        $script:startupLogLines = (Get-Content -LiteralPath $script:logFile | Measure-Object).Count
    }
    Protect-Directory (Join-Path $DataDir 'runtime')
    $bytes = New-Object byte[] 32
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    $script:launchId = -join ($bytes | ForEach-Object { $_.ToString('x2') })
    Write-UTF8 ($script:readyFile+'.request') ($script:launchId+"`n")
    # The service reads this as LocalSystem, independently of its installer.
    Protect-File ($script:readyFile+'.request')
}
function Wait-Ready {
    $deadline = [DateTime]::UtcNow.AddSeconds($StartupTimeout)
    while ([DateTime]::UtcNow -lt $deadline) {
        $current = Service-Record
        if ($current -and $current.ProcessId -gt 0) {
            & $binary --config $config --check-ready --expect-pid $current.ProcessId --expect-launch-id $script:launchId 2>$null
            if ($LASTEXITCODE -eq 0 -and $current.State -eq 'Running') { return }
        }
        Start-Sleep -Seconds 1
    }
    throw 'Service did not confirm this startup before the timeout.'
}
function Commit-Start {
    $script:startupCommitted = $false
    $temporary = $null
    try {
        if ($script:launchId -cnotmatch '^[a-f0-9]{64}$') { throw 'Invalid startup confirmation.' }
        $committed = $script:readyFile+'.committed'
        Assert-DirectoryPath (Split-Path $committed -Parent)
        if (Test-Path -LiteralPath $committed) { Assert-RegularFile $committed }
        $temporary = $committed+'.'+[guid]::NewGuid().ToString('N')
        Write-UTF8 $temporary ($script:launchId+"`n")
        Protect-File $temporary
        # PowerShell converts $null to an empty backup filename for this overload.
        if (Test-Path -LiteralPath $committed) { [IO.File]::Replace($temporary,$committed,[NullString]::Value) }
        else { [IO.File]::Move($temporary,$committed) }
        $script:startupCommitted = $true
    } catch {
        Write-Warning ("Could not confirm the installation for scan cleanup; the service remains available. {0}" -f $_.Exception.GetBaseException().Message) -WarningAction Continue
    } finally {
        if ($temporary) {
            try { Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue }
            catch { Write-Warning 'Could not remove the temporary startup confirmation file.' -WarningAction Continue }
        }
    }
}
function Show-InitialCredentials {
    if ($Role -ne 'master' -or -not (Test-Path -LiteralPath $script:logFile -PathType Leaf)) { return }
    $index = 0
    $password = $null
    $token = $null
    # Only inspect this launch, not credentials retained in older log lines.
    Get-Content -LiteralPath $script:logFile | ForEach-Object {
        $index++
        if ($index -gt $script:startupLogLines -and $_ -cmatch '^Initial password: ([A-Za-z0-9_-]+)$') { $password = $Matches[1] }
        if ($index -gt $script:startupLogLines -and $_ -cmatch '^Open the console to complete setup\. Setup token: ([A-Za-z0-9_-]+)$') { $token = $Matches[1] }
    }
    if ($password) { Write-Host "`nInitial administrator: admin`nInitial password: $password" }
    elseif ($token) { Write-Host "`nSetup token: $token" }
}
try {
    if ($ReleaseUrl) {
        $ReleaseUrl = $ReleaseUrl.TrimEnd('/')
        $uri = [Uri]$ReleaseUrl
        if (-not $uri.IsAbsoluteUri -or $uri.UserInfo -or $uri.Query -or $uri.Fragment -or ($uri.Scheme -ne 'https' -and -not ($uri.Scheme -eq 'http' -and $uri.IsLoopback)) -or $uri.AbsolutePath -notmatch '^/[A-Za-z0-9._~/-]+$') { throw 'Release URL must be an HTTPS version directory (loopback HTTP is allowed for testing).' }
    }
    $manifest = Fetch 'manifest.tsv'
    if ((Get-Item $manifest).Length -ge 65536) { throw 'Manifest too large.' }
    $rows = @(Get-Content -LiteralPath $manifest)
    if (@($rows | Where-Object { $_ -eq "format`toss-transfer-go-v1" }).Count -ne 1) { throw 'Unknown manifest format.' }
    $versions = @($rows | Where-Object { $_.StartsWith("version`t") })
    if ($versions.Count -ne 1) { throw 'Invalid version entry.' }
    $version = ($versions[0] -split "`t")[1]
    if ($version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$') { throw 'Invalid release version.' }
    if ($ReleaseUrl.EndsWith('/latest',[StringComparison]::Ordinal)) {
        if ($version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') { throw 'Invalid latest release version.' }
        $latestHash = (Get-FileHash -LiteralPath $manifest -Algorithm SHA256).Hash
        $ReleaseUrl = $ReleaseUrl.Substring(0,$ReleaseUrl.Length-7) + '/v' + $version
        $manifest = Fetch 'manifest.tsv'
        if ((Get-FileHash -LiteralPath $manifest -Algorithm SHA256).Hash -cne $latestHash) { throw 'Latest and versioned release manifests differ; retry after publication finishes.' }
    }
    $filename = "oss-transfer-$Role-windows-$arch.exe"
    $records = @($rows | Where-Object { $_.StartsWith("file`twindows`t$arch`t$Role`t") })
    if ($records.Count -ne 1) { throw 'Missing or duplicate platform binary.' }
    $record = $records[0] -split "`t"
    if ($record.Count -ne 7 -or $record[4] -cne $filename -or $record[5] -notmatch '^[a-f0-9]{64}$' -or $record[6] -notmatch '^[0-9]+$') { throw 'Invalid binary manifest entry.' }
    $cliFilename = "oss-transfer-cli-windows-$arch.exe"
    $cliRecords = @($rows | Where-Object { $_.StartsWith("file`twindows`t$arch`tcli`t") })
    if ($cliRecords.Count -ne 1) { throw 'Missing or duplicate CLI binary.' }
    $cliRecord = $cliRecords[0] -split "`t"
    if ($cliRecord.Count -ne 7 -or $cliRecord[4] -cne $cliFilename -or $cliRecord[5] -notmatch '^[a-f0-9]{64}$' -or $cliRecord[6] -notmatch '^[0-9]+$') { throw 'Invalid CLI manifest entry.' }
    if (@($rows | Where-Object { $_.StartsWith("archive`t") }).Count -gt 0) {
        $archiveRoot = "oss-transfer-windows-$arch"
        $archiveName = "$archiveRoot.zip"
        $archiveRecords = @($rows | Where-Object { $_.StartsWith("archive`twindows`t$arch`t") })
        if ($archiveRecords.Count -ne 1) { throw 'Missing or duplicate platform archive.' }
        $archiveRecord = $archiveRecords[0] -split "`t"
        if ($archiveRecord.Count -ne 6 -or $archiveRecord[3] -cne $archiveName -or $archiveRecord[4] -notmatch '^[a-f0-9]{64}$' -or $archiveRecord[5] -notmatch '^[0-9]+$') { throw 'Invalid archive manifest entry.' }
        $archivePath = Fetch $archiveName
        if ((Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant() -cne $archiveRecord[4] -or (Get-Item $archivePath).Length -ne [long]$archiveRecord[5]) { throw 'Archive checksum mismatch.' }
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $archive = [IO.Compression.ZipFile]::OpenRead($archivePath)
        try {
            foreach ($binaryRecord in @($record,$cliRecord)) {
                $member = "$archiveRoot/oss-transfer-$($binaryRecord[3]).exe"
                $entries = @($archive.Entries | Where-Object { $_.FullName -ceq $member })
                if ($entries.Count -ne 1) { throw "Archive member must occur once: $member" }
                $entry = $entries[0]
                $kind = ($entry.ExternalAttributes -shr 16) -band 0xF000
                if ($kind -notin @(0,0x8000) -or ($entry.ExternalAttributes -band 0x10) -ne 0 -or $entry.Length -ne [long]$binaryRecord[6]) { throw "Archive member must be a regular file of the expected size: $member" }
                $destination = Join-Path $work $binaryRecord[4]
                $entryStream = $entry.Open()
                try {
                    $targetStream = [IO.File]::Create($destination)
                    try { $entryStream.CopyTo($targetStream) } finally { $targetStream.Dispose() }
                } finally { $entryStream.Dispose() }
            }
        } finally { $archive.Dispose() }
        $candidate = Join-Path $work $filename
        $cliCandidate = Join-Path $work $cliFilename
    } else {
        # Older releases publish individual binaries instead of archive rows.
        $candidate = Fetch $filename
        $cliCandidate = Fetch $cliFilename
    }
    if ((Get-FileHash -LiteralPath $candidate -Algorithm SHA256).Hash.ToLowerInvariant() -cne $record[5] -or (Get-Item $candidate).Length -ne [long]$record[6]) { throw 'Binary checksum mismatch.' }
    if ((Get-FileHash -LiteralPath $cliCandidate -Algorithm SHA256).Hash.ToLowerInvariant() -cne $cliRecord[5] -or (Get-Item $cliCandidate).Length -ne [long]$cliRecord[6]) { throw 'CLI checksum mismatch.' }
    $versionOutput = Invoke-Native $candidate @('--version')
    if (-not $versionOutput.StartsWith("oss-transfer-$Role $version ")) { throw 'Binary version or platform check failed.' }
    $cliVersion = (Invoke-Native $cliCandidate @('--version','--json') | ConvertFrom-Json).version
    if ($cliVersion -cne $version) { throw 'CLI version or platform check failed.' }
    if ($existing -or $AdoptConfig) {
        $sourceConfig = if ($AdoptConfig) { $AdoptConfig } else { $config }
        Assert-RegularFile $sourceConfig
        $configLines = @(Invoke-Native $candidate @('--config',$sourceConfig,'--check-config'))
        $fields = @{}
        foreach ($line in $configLines) { $pair=$line -split "`t",2; $fields[$pair[0]]=$pair[1] }
        if ($fields.serviceName -ne $ServiceName -or $fields.dataDirectory -ne $DataDir) { throw 'Configured service/data paths differ; use matching installer arguments.' }
        $script:readyFile = $fields.readyFile
        $script:logFile = $fields.logFile
        Assert-RegularFile $database
        if ((Get-Item -LiteralPath $database).Length -eq 0) { throw 'Existing Go database is empty; refusing to initialize a replacement.' }
        if ($AdoptConfig) {
            $freshConfig = Join-Path $work 'service.json'
            Copy-Item -LiteralPath $sourceConfig -Destination $freshConfig
        }
    } else {
        if ($Role -eq 'agent' -and (-not $MasterUrl -or -not $env:TRANSFER_JOIN_TOKEN)) { throw 'Copy an Agent join command from Nodes > Add node.' }
        $script:readyFile = Join-Path $DataDir 'runtime\ready'
        $script:logFile = Join-Path $DataDir 'logs\service.log'
        $settings = @{ serviceName=$ServiceName; dataDirectory=$DataDir; readyFile=$script:readyFile; logFile=$script:logFile }
        if ($Role -eq 'master') { $settings.listen = $Listen }
        $freshConfig = Join-Path $work 'service.json'
        Write-UTF8 $freshConfig ($settings | ConvertTo-Json)
        Invoke-Native $candidate @('--config',$freshConfig,'--check-config') | Out-Null
    }
    if ((Test-Path (Join-Path $DataDir 'CURRENT')) -or (Test-Path (Join-Path $DataDir 'rocksdb\CURRENT'))) { throw 'RocksDB requires an explicit migration.' }
    if (-not $Yes -and (Read-Host "Install $Role $version? [y/N]") -notmatch '^[yY]$') { return }
    if ($existing -or $AdoptConfig -or $hadCli -or $hadCliSidecars -or $hadDataCLIConnections) {
        Prepare-BackupDirectory
        if ($existing) { Maintain-Backups 'prune' }
        Test-BackupSpace
    }
    foreach ($path in $directories) { Protect-Directory $path }
    if ($existing) {
        $backup = New-TemporaryBackup
        Copy-Item -LiteralPath $binary -Destination (Join-Path $backup 'binary.exe')
        Copy-Item -LiteralPath $config -Destination (Join-Path $backup 'service.json')
        Copy-Item -LiteralPath $metadata -Destination (Join-Path $backup 'install.json')
        $oldService = Service-Record
        if ($oldService) {
            if ($oldService.StartName -ne 'LocalSystem') { throw 'Review the custom service account before upgrading.' }
            Write-UTF8 (Join-Path $backup 'service-record.json') ($oldService | Select-Object PathName,StartMode | ConvertTo-Json)
            $wasRunning = $oldService.State -ne 'Stopped'
        }
        if ($Role -eq 'agent') {
            Write-Host 'Protecting Agent tasks during the upgrade...'
            Invoke-Native $candidate @('--config',$config,'--upgrade-maintenance','begin','--upgrade-maintenance-file',(Join-Path $work 'agent-maintenance.json')) | Out-Null
            $agentMaintenanceStarted = $true
        }
        $changed = $true
        Write-Host "Stopping $ServiceName before the database backup..."
        Stop-OwnedService
        Invoke-Native $candidate @('--config',(Join-Path $backup 'service.json'),'--backup-state',(Join-Path $backup 'database')) | Out-Null
        $hadDatabaseBackup = $true
    } elseif ($AdoptConfig) {
        $backup = New-TemporaryBackup
        Copy-Item -LiteralPath $freshConfig -Destination (Join-Path $backup 'service.json')
        Invoke-Native $candidate @('--config',$freshConfig,'--backup-state',(Join-Path $backup 'database')) | Out-Null
        $hadDatabaseBackup = $true
    }
    if ($hadCli -or $hadCliSidecars -or $hadDataCLIConnections) {
        if (-not $backup) {
            $backup = New-TemporaryBackup
        }
        if ($hadCli) { Copy-Item -LiteralPath $CliPath -Destination (Join-Path $backup 'cli.exe') }
        Backup-CLIConnections
    }
    $changed = $true
    Write-Host "Installing $Role $version..."
    Copy-Item -LiteralPath $candidate -Destination ($binary+'.new')
    Move-Item -LiteralPath ($binary+'.new') -Destination $binary -Force
    [IO.Directory]::CreateDirectory((Split-Path $CliPath -Parent)) | Out-Null
    Copy-Item -LiteralPath $cliCandidate -Destination ($CliPath+'.new')
    Move-Item -LiteralPath ($CliPath+'.new') -Destination $CliPath -Force
    $cliChanged = $true
    if (-not $existing) { Copy-Item -LiteralPath $freshConfig -Destination $config }
    Write-UTF8 $metadata (@{role=$Role; serviceName=$ServiceName; config=$config; version=$version; cliPath=$CliPath; backupDir=$BackupDir; dataDir=$DataDir} | ConvertTo-Json)
    if (-not $existing -and -not $AdoptConfig -and $Role -eq 'agent') { $env:OSS_TRANSFER_JOIN_TOKEN = $env:TRANSFER_JOIN_TOKEN; Invoke-Native $binary @('--config',$config,'--master-url',$MasterUrl,'--enroll-only') | Out-Null }
    Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
    Remove-Item Env:OSS_TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
    $command = '"'+$binary+'" --config "'+$config+'"'
    if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
        $result = Invoke-CimMethod -InputObject (Service-Record) -MethodName Change -Arguments @{PathName=$command}
        if ($result.ReturnValue -ne 0) { throw 'Could not update the service command.' }
    } else { New-Service -Name $ServiceName -BinaryPathName $command -DisplayName "OSS Transfer $Role" -StartupType Automatic | Out-Null }
    Prepare-Start
    Start-Service -Name $ServiceName
    Wait-Ready
    if ($agentMaintenanceStarted) {
        try {
            Invoke-Native $candidate @('--config',$config,'--upgrade-maintenance','end','--upgrade-maintenance-file',(Join-Path $work 'agent-maintenance.json')) | Out-Null
            $agentMaintenanceStarted = $false
        } catch { Write-Warning 'Could not end Agent upgrade maintenance; it expires automatically within 30 minutes.' }
    }
    Write-Host "Installed $Role $version. Service: $ServiceName"
    Write-Host "Configuration: $config"
    Write-Host "CLI: $CliPath"
    Write-Host ("Tasks: & '"+$CliPath.Replace("'","''")+"' task list")
    Write-Host ('Log: '+$script:logFile)
    Show-InitialCredentials
    $installationSucceeded = $true
} catch {
    $failure = $_
    # Preserve the Win32 cause before rollback deletes a failed first service.
    # Diagnostic access must never replace the original installation exception.
    try {
        $cause = $failure.Exception
        while ($cause) {
            $native = $cause.PSObject.Properties['NativeErrorCode']
            $code = if ($native) { $native.Value } else { $cause.HResult }
            Write-Warning ("Startup error: {0}; code: {1}; {2}" -f $cause.GetType().FullName,$code,$cause.Message)
            $cause = $cause.InnerException
        }
    } catch { Write-Warning 'Could not inspect the original exception.' }
    if ($changed) {
        try {
            $failedService = Service-Record
            if ($failedService) {
                Write-Warning ("Service state: {0}; Windows exit code: {1}; service exit code: {2}" -f $failedService.State,$failedService.ExitCode,$failedService.ServiceSpecificExitCode)
                Write-Warning ("Service account: {0}; command: {1}" -f $failedService.StartName,$failedService.PathName)
            }
        } catch { Write-Warning 'Could not read the failed service record.' }
        Write-Warning 'Installation failed; restoring the previous installation.'
        try {
            Stop-OwnedService
            if ($cliChanged) {
                if ($hadCli) { Copy-Item -LiteralPath (Join-Path $backup 'cli.exe') -Destination $CliPath -Force }
                else { Remove-Item -LiteralPath $CliPath -Force }
                Restore-CLIConnections
            }
            if ($existing) {
                if ($hadDatabaseBackup) { Invoke-Native $candidate @('--config',(Join-Path $backup 'service.json'),'--restore-state',(Join-Path $backup 'database')) | Out-Null }
                Copy-Item -LiteralPath (Join-Path $backup 'binary.exe') -Destination $binary -Force
                Copy-Item -LiteralPath (Join-Path $backup 'service.json') -Destination $config -Force
                Copy-Item -LiteralPath (Join-Path $backup 'install.json') -Destination $metadata -Force
                if ($oldService) {
                    $result = Invoke-CimMethod -InputObject (Service-Record) -MethodName Change -Arguments @{PathName=$oldService.PathName}
                    if ($result.ReturnValue -ne 0) { throw 'Could not restore the service command.' }
                } else {
                    $created = Service-Record
                    if ($created) { Invoke-CimMethod -InputObject $created -MethodName Delete | Out-Null }
                }
                if ($wasRunning) {
                    Prepare-Start; Start-Service -Name $ServiceName; Wait-Ready
                    if ($agentMaintenanceStarted) {
                        Invoke-Native $candidate @('--config',$config,'--upgrade-maintenance','end','--upgrade-maintenance-file',(Join-Path $work 'agent-maintenance.json')) | Out-Null
                        $agentMaintenanceStarted = $false
                    }
                }
            } else {
                $created = Service-Record
                if ($created) { Invoke-CimMethod -InputObject $created -MethodName Delete | Out-Null }
                if ($AdoptConfig -and $hadDatabaseBackup) {
                    Invoke-Native $candidate @('--config',$freshConfig,'--restore-state',(Join-Path $backup 'database')) | Out-Null
                    Write-Warning "Restored the adopted database. Backup: $backup"
                }
                Write-Warning 'Installation failed. Existing files were retained for diagnosis.'
                Write-Warning ('Service log, if created: '+(Join-Path $DataDir 'logs\service.log'))
            }
        } catch { Write-Warning ('Rollback needs attention: '+$_.Exception.Message) }
    }
    if ($backup) { Write-Warning "Temporary backup retained at $backup" -WarningAction Continue }
    throw $failure
} finally {
    # This finally block is outside the catch that restores the old database.
    Finish-TemporaryBackup
    Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
    Remove-Item Env:OSS_TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}

} finally {
    if ($sourceWork) { Remove-Item -LiteralPath $sourceWork -Recurse -Force -ErrorAction SilentlyContinue }
}
