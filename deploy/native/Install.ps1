# Native Go service installation. Run from an elevated PowerShell session.
[CmdletBinding()]
param(
    [ValidateSet('master','agent')][string]$Role,
    [ValidateSet('auto','github','jdoss')][string]$Source = 'auto',
    [ValidateSet('en','zh')][string]$Lang,
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
    [string]$RootDir,
    [string]$ServiceName,
    [string]$Listen = '0.0.0.0:1685',
    [string]$MasterUrl,
    [switch]$Upgrade,
    [switch]$WithAgent,
    [switch]$WithoutAgent,
    [switch]$Yes,
    [ValidateRange(10,600)][int]$StartupTimeout = 120
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
function Resolve-InstallLanguage([string]$Requested,[string]$Inherited,[string]$Culture) {
    if ($Requested -in @('en','zh')) { return $Requested }
    if ($Inherited -in @('en','zh')) { return $Inherited }
    if ($Culture.StartsWith('zh',[StringComparison]::OrdinalIgnoreCase)) { return 'zh' }
    return 'en'
}
function Get-InstallText([string]$English,[string]$Chinese) {
    if ($Lang -eq 'zh') { return $Chinese }
    return $English
}
$Lang = Resolve-InstallLanguage $Lang $env:OSS_TRANSFER_INSTALL_LANG (Get-UICulture).Name
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
function Test-MasterPackageUrl([string]$Value,[string]$Origin) {
    $uri = $null; $master = $null
    if (-not [Uri]::TryCreate($Value,[UriKind]::Absolute,[ref]$uri) -or -not [Uri]::TryCreate($Origin,[UriKind]::Absolute,[ref]$master)) { return $false }
    return $uri.Scheme -in @('http','https') -and -not $uri.UserInfo -and -not $uri.Query -and -not $uri.Fragment -and
        $uri.GetLeftPart([UriPartial]::Authority) -ceq $master.GetLeftPart([UriPartial]::Authority) -and
        -not $master.UserInfo -and -not $master.Query -and -not $master.Fragment -and $master.AbsolutePath -eq '/' -and
        $uri.AbsolutePath -cmatch '^/api/agents/bootstrap/[A-Za-z0-9_-]+/files$'
}
function Test-ReleaseDownloadUrl([string]$Value,[string]$Origin,[string]$AgentMaster) {
    $uri = $null
    if (-not [Uri]::TryCreate($Value,[UriKind]::Absolute,[ref]$uri) -or $uri.UserInfo -or $uri.Query -or $uri.Fragment) { return $false }
    if (Test-MasterPackageUrl $Origin $AgentMaster) {
        # A join link may use a private HTTP address; its downloads must stay
        # under that exact invitation rather than redirecting credentials.
        $base = [Uri]($Origin.TrimEnd('/')+'/')
        return $uri.GetLeftPart([UriPartial]::Authority) -ceq $base.GetLeftPart([UriPartial]::Authority) -and
            $uri.AbsolutePath.StartsWith($base.AbsolutePath,[StringComparison]::Ordinal) -and
            $uri.AbsolutePath.Substring($base.AbsolutePath.Length) -cmatch '^[A-Za-z0-9._-]+$'
    }
    return $uri.Scheme -eq 'https' -or ($uri.Scheme -eq 'http' -and $uri.IsLoopback)
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
        Write-Warning ((Get-InstallText 'Could not reach {0}; trying {1}.' '无法连接 {0}，正在尝试 {1}。') -f $selected,$alternate)
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
        foreach ($assetName in @('install.sh','native-install.sh','Install.ps1')) {
            $paths = switch ($assetName) {
                'native-install.sh' { @('deploy/native/install.sh','deploy/go/install.sh') }
                'Install.ps1' { @('deploy/native/Install.ps1','deploy/go/Install.ps1') }
                default { @('install.sh') }
            }
            $assetPath = Join-Path $Directory $assetName
            $downloaded = $false
            foreach ($path in $paths) {
                try {
                    Invoke-SourceRequest "https://raw.githubusercontent.com/MoonWX/oss-transfer/$($fields['FullCommit'])/$path" 60 $assetPath
                    $downloaded = $true
                    break
                } catch [Net.WebException] { }
            }
            if (-not $downloaded) { throw "Could not download the pinned installer: $assetName" }
            $assetSize = (Get-Item -LiteralPath $assetPath).Length
            if ($assetSize -lt 1 -or $assetSize -gt 1048576) { throw 'Installer has an invalid size.' }
            $assetHash = (Get-FileHash -LiteralPath $assetPath -Algorithm SHA256).Hash.ToLowerInvariant()
            $manifest.Add("asset`t$assetName`t$assetHash`t$assetSize")
        }
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
if ($WithAgent -and $WithoutAgent) { throw 'Choose -WithAgent or -WithoutAgent.' }
if (($WithAgent -or $WithoutAgent) -and $Role -and $Role -ne 'master') { throw 'The local Agent option belongs to a Master installation.' }
$rootExplicit = $PSBoundParameters.ContainsKey('RootDir')
if (-not $RootDir) { $RootDir = Join-Path $env:ProgramData 'OSS Transfer' }
# A repeated install repairs the original installation instead of creating a
# second database under the new layout. Explicit paths still take precedence.
if (-not $Upgrade -and -not $AdoptConfig) {
    $selectedRole = if ($Role) { $Role } else { 'master' }
    $locations = if ($InstallDir) { @($InstallDir) } else {
        @((Join-Path $RootDir "$selectedRole\bin")) + $(if (-not $rootExplicit) { @((Join-Path $env:ProgramFiles "OSS Transfer\$selectedRole")) } else { @() })
    }
    foreach ($directory in $locations) {
        $recordPath = Join-Path $directory 'install.json'
        if (Test-Path -LiteralPath $recordPath -PathType Leaf) {
            if ((Get-Item -LiteralPath $recordPath -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Installation record must not be linked.' }
            $saved = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json
            if ($saved.role -ne $selectedRole) { throw 'Existing installation role differs.' }
            if ($ServiceName -and $saved.serviceName -ne $ServiceName) { throw 'Existing service name differs from -ServiceName.' }
            $InstallDir = $directory; $Upgrade = $true
            Write-Host (Get-InstallText 'Existing installation found; preserving its configuration and data.' '检测到已有安装，保留配置和数据。')
            break
        }
    }
}
$architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
$arch = switch ($architecture) { 'x64' {'amd64'} 'arm64' {'arm64'} default {throw 'Unsupported CPU architecture.'} }
if (-not $ReleaseUrl -and -not $PackageDir) {
    $resolved = Resolve-DownloadSource $Source $Version $arch
    Write-Host ((Get-InstallText 'Download source: {0} ({1})' '下载源：{0}（{1}）') -f $resolved.Source,$resolved.Version)
    if ($resolved.Source -eq 'github') {
        $sourceWork = Join-Path ([IO.Path]::GetTempPath()) ('oss-transfer-source-'+[Guid]::NewGuid().ToString('N'))
        Protect-Directory $sourceWork
        New-GitHubPackage $resolved $sourceWork
        $PackageDir = $sourceWork
    } else { $ReleaseUrl = $resolved.ReleaseUrl }
}

function Read-InstallationRoot($Record,[string]$Directory) {
    if (-not $Record.PSObject.Properties['rootDir'] -or -not $Record.rootDir) { return '' }
    $savedRoot = [string]$Record.rootDir
    $roleBase = Join-Path $savedRoot $Record.role
    if (-not $Directory.Equals((Join-Path $roleBase 'bin'),[StringComparison]::OrdinalIgnoreCase) -or
        -not ([string]$Record.config).Equals((Join-Path (Join-Path $roleBase 'config') 'service.json'),[StringComparison]::OrdinalIgnoreCase) -or
        -not $Record.PSObject.Properties['dataDir'] -or
        -not ([string]$Record.dataDir).Equals((Join-Path $roleBase 'data'),[StringComparison]::OrdinalIgnoreCase)) {
        throw 'Recorded installation root does not match its configuration and data directories.'
    }
    return $savedRoot
}
if ($Upgrade) {
    if (-not $InstallDir) {
        $candidates = @{}
        function Add-Installation([string]$Directory) {
            if ($rootExplicit -and -not $Directory.StartsWith($RootDir+'\',[StringComparison]::OrdinalIgnoreCase)) { return }
            $recordPath = Join-Path $Directory 'install.json'
            if (-not (Test-Path -LiteralPath $recordPath -PathType Leaf)) { return }
            if ((Get-Item -LiteralPath $recordPath -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { return }
            $record = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json
            if ($record.role -notin @('master','agent') -or ($Role -and $record.role -ne $Role) -or ($ServiceName -and $record.serviceName -ne $ServiceName)) { return }
            $candidates[$Directory] = $record
        }
        foreach ($installedRole in @('master','agent')) {
            Add-Installation (Join-Path $RootDir "$installedRole\bin")
            if (-not $rootExplicit) { Add-Installation (Join-Path $env:ProgramFiles "OSS Transfer\$installedRole") }
        }
        foreach ($installedService in @(Get-CimInstance Win32_Service)) {
            if ($installedService.PathName -match '^"([^"]+\\oss-transfer-(?:master|agent)\.exe)"(?:\s|$)') {
                Add-Installation (Split-Path $Matches[1] -Parent)
            }
        }
        if ($candidates.Count -eq 0) { throw 'No managed installation found; use -InstallDir for an installation without a service definition.' }
        Write-Host (Get-InstallText 'Upgrade installations:' '将升级以下服务：')
        foreach ($directory in @($candidates.Keys | Sort-Object)) { Write-Host "  $($candidates[$directory].serviceName): $directory" }
        if (-not $Yes -and (Read-Host (Get-InstallText 'Back up and upgrade these installations? [y/N]' '备份并升级这些服务？[y/N]')) -notmatch '^[yY]$') { return }
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
            $childArguments.Lang = $Lang
            & $PSCommandPath @childArguments
        }
        return
    }
    $recordPath = Join-Path $InstallDir 'install.json'
    if (-not (Test-Path -LiteralPath $recordPath -PathType Leaf) -or ((Get-Item -LiteralPath $recordPath -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'No managed installation found in the selected directory.' }
    $record = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json
    if ($record.role -notin @('master','agent') -or ($Role -and $Role -ne $record.role)) { throw 'Installed role differs from -Role.' }
    $savedRoot = Read-InstallationRoot $record $InstallDir
    if (-not $rootExplicit -and $savedRoot) { $RootDir = $savedRoot }
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
$base = Join-Path $RootDir $Role
if (-not $InstallDir) { $InstallDir = Join-Path $base 'bin' }
if (-not $ConfigDir) { $ConfigDir = Join-Path $base 'config' }
if (-not $DataDir) { $DataDir = Join-Path $base 'data' }
$autoAdopt = $false
$retainedConfig = Join-Path $ConfigDir 'service.json'
if (-not $Upgrade -and -not $AdoptConfig -and (Test-Path -LiteralPath $retainedConfig -PathType Leaf)) {
    if ((Get-Item -LiteralPath $retainedConfig -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Retained configuration must not be linked.' }
    $savedConfig = Get-Content -LiteralPath $retainedConfig -Raw | ConvertFrom-Json
    if (-not $PSBoundParameters.ContainsKey('DataDir')) { $DataDir = $savedConfig.dataDirectory }
    if (-not $PSBoundParameters.ContainsKey('ServiceName')) { $ServiceName = $savedConfig.serviceName }
    if ($ServiceName -notmatch '^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$') { throw 'Invalid retained service name.' }
    if (-not $PSBoundParameters.ContainsKey('InstallDir')) {
        $savedService = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue
        $commandPattern = '^"([^"]+\\oss-transfer-'+$Role+'\.exe)" --config "'+[Regex]::Escape($retainedConfig)+'"$'
        if ($savedService -and $savedService.PathName -match $commandPattern) {
            $savedBin = Split-Path $Matches[1] -Parent
            if (-not $rootExplicit -or $savedBin.StartsWith($RootDir+'\',[StringComparison]::OrdinalIgnoreCase)) { $InstallDir = $savedBin }
        }
    }
    $AdoptConfig = $retainedConfig; $autoAdopt = $true
    Write-Host (Get-InstallText 'Retained configuration found; restoring this installation with its existing data.' '检测到保留的配置，使用原有数据恢复安装。')
}
if ($ServiceName -notmatch '^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$') { throw 'Invalid retained service name.' }
function Resolve-BackupDirectory([string]$Requested,[string]$RoleName,[string]$InstallationRoot = '') {
    if ($Requested) { return $Requested }
    if (-not $InstallationRoot) { $InstallationRoot = Join-Path $env:ProgramData 'OSS Transfer' }
    return Join-Path (Join-Path $InstallationRoot $RoleName) 'backups'
}
$BackupDir = Resolve-BackupDirectory $BackupDir $Role $RootDir
if ($Role -eq 'master' -and -not $Upgrade -and -not $AdoptConfig -and -not $WithAgent -and -not $WithoutAgent) {
    if ($Yes) { $WithAgent = $true }
    else { $WithAgent = (Read-Host (Get-InstallText 'Install an Agent on this host as well? [Y/n]' '是否同时安装本机 Agent？[Y/n]')) -notmatch '^[nN]$' }
}
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
Assert-DirectoryPath $RootDir
foreach ($path in $directories) { Assert-DirectoryPath $path }
for ($i=0; $i -lt 4; $i++) { for ($j=0; $j -lt 4; $j++) {
    if ($i -ne $j -and ($directories[$i].Equals($directories[$j],[StringComparison]::OrdinalIgnoreCase) -or $directories[$i].StartsWith($directories[$j]+'\',[StringComparison]::OrdinalIgnoreCase))) { throw 'Binary, config, data and backup directories must be separate.' }
} }
function Assert-RegularFile([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf) -or ((Get-Item -LiteralPath $Path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Expected a regular file: $Path" }
}
function Read-PublishedMasterOrigin([string]$Directory) {
    $path = Join-Path $Directory 'cli-cluster.json'
    Assert-RegularFile $path
    if ((Get-Item -LiteralPath $path).Length -gt 65536) { throw 'Master local connection file is too large.' }
    $connection = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
    $origin = [string]$connection.masterUrl
    $uri = $null
    if ($connection.role -ne 'master' -or -not [Uri]::TryCreate($origin,[UriKind]::Absolute,[ref]$uri) -or
        $uri.Scheme -notin @('http','https') -or $uri.UserInfo -or $uri.Query -or $uri.Fragment -or $uri.AbsolutePath -ne '/') { throw 'Master local connection was not published correctly.' }
    # Preserve the actual listener's origin, including [::1]. Guessing an IPv4
    # loopback from 0.0.0.0 would no longer match the private CLI credential.
    return $origin
}
function Resolve-LocalAgentRoot([string]$SelectedRoot,[bool]$ExplicitRoot,[string]$MasterInstall,[string]$LegacyMaster) {
    if ($ExplicitRoot) { return $SelectedRoot }
    $suffix = '\master\bin'
    if ($MasterInstall.EndsWith($suffix,[StringComparison]::OrdinalIgnoreCase)) {
        return $MasterInstall.Substring(0,$MasterInstall.Length-$suffix.Length)
    }
    if ($MasterInstall.Equals($LegacyMaster,[StringComparison]::OrdinalIgnoreCase)) { return $SelectedRoot }
    # A custom flat installation gets a sibling Agent directory, not a node
    # silently installed in an unrelated default cluster.
    $separator = $MasterInstall.LastIndexOf('\')
    if ($separator -lt 0) { throw 'Master installation path must be absolute.' }
    return $MasterInstall.Substring(0,$separator)+'\oss-transfer-local-agent'
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
$hadBinary = Test-Path -LiteralPath $binary
if ($hadBinary) { Assert-RegularFile $binary }
$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existing) {
    Assert-RegularFile $metadata; Assert-RegularFile $config
    $installed = Get-Content -LiteralPath $metadata -Raw | ConvertFrom-Json
    if ($installed.role -ne $Role -or $installed.serviceName -ne $ServiceName -or $installed.config -ne $config) { throw 'Existing installation identity does not match.' }
} else {
    if ($Upgrade) { throw 'No managed installation found; V4.0.x requires database migration first.' }
    if ($service) {
        $retainedService = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'"
        $expectedCommand = '"'+$binary+'" --config "'+$config+'"'
        if (-not $autoAdopt -or $retainedService.PathName -cne $expectedCommand -or $retainedService.StartName -ne 'LocalSystem') { throw 'An unmanaged service already exists.' }
    }
    foreach ($path in @($InstallDir,$ConfigDir,$DataDir)) {
        if ($AdoptConfig -and $path -eq $DataDir) { continue }
        if ($autoAdopt -and $path -eq $ConfigDir) { continue }
        if ($autoAdopt -and $path -eq $InstallDir) {
            foreach ($entry in @(Get-ChildItem -LiteralPath $path -Force -ErrorAction SilentlyContinue)) {
                if ($entry.Name -notin @("oss-transfer-$Role.exe",'oss-transfer-cli.exe','oss-transfer-cli.exe.cluster.json','oss-transfer-cli.exe.local-auth.json') -or $entry.PSIsContainer -or ($entry.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Unexpected file in retained installation: $($entry.FullName)" }
            }
            continue
        }
        if ((Test-Path -LiteralPath $path) -and @(Get-ChildItem -LiteralPath $path -Force).Count -gt 0) { throw "Refusing nonempty directory: $path" }
    }
}
if ($existing -and $installed.PSObject.Properties['cliPath'] -and $installed.cliPath -ne $CliPath) { throw 'CLI path differs from the installed path; keep -CliPath unchanged during upgrade.' }
$retainedCLI = $false
if ($AdoptConfig -and $hadCliSidecars -and $hadDataCLIConnections) {
    $retainedCLI = $true
    foreach ($suffix in @('cluster.json','local-auth.json')) {
        $sidecar = $CliPath+'.'+$suffix
        $dataSidecar = Join-Path $DataDir ('cli-'+$suffix)
        if (-not (Test-Path -LiteralPath $sidecar) -or -not (Test-Path -LiteralPath $dataSidecar) -or
            (Get-FileHash -LiteralPath $sidecar -Algorithm SHA256).Hash -cne (Get-FileHash -LiteralPath $dataSidecar -Algorithm SHA256).Hash) { $retainedCLI = $false; break }
    }
}
if (($hadCli -or $hadCliSidecars) -and (-not $existing -or -not $installed.PSObject.Properties['cliPath'] -or $installed.cliPath -ne $CliPath) -and -not $ReplaceCli -and -not $retainedCLI -and -not ($autoAdopt -and $CliPath -ceq (Join-Path $InstallDir 'oss-transfer-cli.exe'))) { throw "CLI already exists: $CliPath; use -ReplaceCli to back it up and replace it, or choose -CliPath." }
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
$masterPackageStage = $null
function Fetch([string]$Name) {
    $target = Join-Path $work $Name
    if ($PackageDir) { $source=Join-Path $PackageDir $Name; Assert-RegularFile $source; Copy-Item -LiteralPath $source -Destination $target }
    else {
        $response = Invoke-WebRequest -UseBasicParsing -Uri "$ReleaseUrl/$Name" -OutFile $target -PassThru -TimeoutSec 600
        if ($response.BaseResponse.PSObject.Properties['ResponseUri']) { $finalUri = $response.BaseResponse.ResponseUri } else { $finalUri = $response.BaseResponse.RequestMessage.RequestUri }
        if (-not (Test-ReleaseDownloadUrl ([string]$finalUri) $ReleaseUrl $MasterUrl)) { throw 'Release download redirected outside its trusted source.' }
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
function Prepare-MasterPackages {
    if ($Role -ne 'master') { return }
    $script:masterPackageStage = Join-Path $work 'packages'
    [IO.Directory]::CreateDirectory($script:masterPackageStage) | Out-Null
    Copy-Item -LiteralPath $manifest -Destination (Join-Path $script:masterPackageStage 'manifest.tsv')
    foreach ($assetName in @('install.sh','native-install.sh','Install.ps1')) {
        $assets = @($rows | Where-Object { $_.StartsWith("asset`t$assetName`t") })
        # Older release manifests do not describe every installer. Their
        # original upstream join workflow remains available.
        if ($assets.Count -eq 0) { continue }
        if ($assets.Count -ne 1) { throw 'Duplicate installer asset.' }
        $asset = $assets[0] -split "`t"
        if ($asset.Count -ne 4 -or $asset[2] -cnotmatch '^[a-f0-9]{64}$' -or $asset[3] -cnotmatch '^[1-9][0-9]{0,6}$' -or [long]$asset[3] -gt 1048576) { throw 'Invalid installer asset.' }
        $source = Fetch $assetName
        if ((Get-Item -LiteralPath $source).Length -ne [long]$asset[3] -or (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant() -cne $asset[2]) { throw 'Installer checksum mismatch.' }
        Copy-Item -LiteralPath $source -Destination (Join-Path $script:masterPackageStage $assetName)
    }
    if (Get-Variable -Name archivePath -ErrorAction SilentlyContinue) {
        Copy-Item -LiteralPath $archivePath -Destination (Join-Path $script:masterPackageStage $archiveName)
    }
    if ($PackageDir) {
        foreach ($line in @($rows | Where-Object { $_.StartsWith("archive`t") })) {
            $entry = $line -split "`t"
            if ($entry.Count -ne 6 -or $entry[1] -cnotmatch '^[a-z0-9]+$' -or $entry[2] -cnotmatch '^[a-z0-9]+$' -or $entry[4] -cnotmatch '^[a-f0-9]{64}$' -or $entry[5] -cnotmatch '^[1-9][0-9]{0,9}$' -or [long]$entry[5] -gt 2147483647) { throw 'Invalid cached archive entry.' }
            $extension = if ($entry[1] -eq 'windows') { 'zip' } else { 'tar.gz' }
            if ($entry[3] -cne "oss-transfer-$($entry[1])-$($entry[2]).$extension") { throw 'Invalid cached archive name.' }
            $source = Join-Path $PackageDir $entry[3]
            if (-not (Test-Path -LiteralPath $source)) { continue }
            Assert-RegularFile $source
            if ((Get-Item -LiteralPath $source).Length -ne [long]$entry[5] -or (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant() -cne $entry[4]) { throw 'Cached archive checksum mismatch.' }
            Copy-Item -LiteralPath $source -Destination (Join-Path $script:masterPackageStage $entry[3])
        }
    }
}
function Publish-MasterPackages {
    if (-not $masterPackageStage) { return }
    $destination = Join-Path $DataDir 'packages'
    $incoming = Join-Path $DataDir ('.packages-new-'+[Guid]::NewGuid().ToString('N'))
    $previous = Join-Path $DataDir ('.packages-old-'+[Guid]::NewGuid().ToString('N'))
    Assert-DirectoryPath $destination
    Protect-Directory $incoming
    try {
        Get-ChildItem -LiteralPath $masterPackageStage -File | Copy-Item -Destination $incoming
        if (Test-Path -LiteralPath $destination) { Move-Item -LiteralPath $destination -Destination $previous }
        try { Move-Item -LiteralPath $incoming -Destination $destination }
        catch {
            if (Test-Path -LiteralPath $previous) { Move-Item -LiteralPath $previous -Destination $destination }
            throw
        }
        if (Test-Path -LiteralPath $previous) { Remove-Item -LiteralPath $previous -Recurse -Force }
    } finally {
        if (Test-Path -LiteralPath $incoming) { Remove-Item -LiteralPath $incoming -Recurse -Force }
    }
}
function Service-Record { Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue }
function Maintain-Backups([string]$Action) {
    $script:backupMaintenanceSucceeded = $false
    try {
        $arguments = @('--backup-maintenance',$Action,'--backup-dir',$BackupDir,'--config',$config)
        if ($Action -eq 'complete') { $arguments += @('--backup-entry',[IO.Path]::GetFileName($backup)) }
        $result = (Invoke-Native $candidate $arguments | Out-String) | ConvertFrom-Json
        if ($Action -eq 'prune' -and $result.count -gt 0) {
            Write-Host ((Get-InstallText 'Removed {0} completed temporary backup(s).' '已删除 {0} 份已完成的临时备份。') -f $result.count)
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
    Write-Host ((Get-InstallText 'Temporary backup: {0}' '临时备份：{0}') -f $path)
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
            if ($file.Name -notin @('database','database.key','service.json','binary.exe','cli.exe','cli.cluster.json','cli.local-auth.json','data-cli.cluster.json','data-cli.local-auth.json')) { throw 'Unexpected temporary backup contents.' }
            Assert-RegularFile $file.FullName
        }
        Remove-Item -LiteralPath $backup -Recurse -Force
        Write-Host ((Get-InstallText 'Removed temporary backup: {0}' '已删除临时备份：{0}') -f $backup)
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
    Write-Host ((Get-InstallText 'Waiting for {0} to become ready...' '正在等待 {0} 就绪...') -f $ServiceName)
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
    if ($password) {
        Write-Host (Get-InstallText "`nInitial administrator: admin" "`n初始管理员：admin")
        Write-Host ((Get-InstallText 'Initial password: {0}' '初始密码：{0}') -f $password)
    } elseif ($token) { Write-Host ((Get-InstallText "`nSetup token: {0}" "`n初始化密钥：{0}") -f $token) }
}
function Get-LocalAccessAddress {
    $candidates = @()
    try {
    foreach ($network in [Net.NetworkInformation.NetworkInterface]::GetAllNetworkInterfaces()) {
        if ($network.OperationalStatus -ne [Net.NetworkInformation.OperationalStatus]::Up -or
            $network.NetworkInterfaceType -in @([Net.NetworkInformation.NetworkInterfaceType]::Loopback,[Net.NetworkInformation.NetworkInterfaceType]::Tunnel)) { continue }
        $properties = $network.GetIPProperties()
        foreach ($unicast in $properties.UnicastAddresses) {
            $address = $unicast.Address
            if ([Net.IPAddress]::IsLoopback($address) -or $address.Equals([Net.IPAddress]::Any) -or
                $address.Equals([Net.IPAddress]::IPv6Any) -or $address.IsIPv6LinkLocal -or
                $address.ToString().StartsWith('169.254.')) { continue }
            $priority = if ($address.AddressFamily -eq [Net.Sockets.AddressFamily]::InterNetwork) { 0 } else { 2 }
            if (@($properties.GatewayAddresses).Count -eq 0) { $priority++ }
            $candidates += [PSCustomObject]@{priority=$priority;address=$address.ToString()}
        }
    }
    } catch { return 'localhost' }
    $selected = $candidates | Sort-Object priority,address | Select-Object -First 1
    if ($selected) { return $selected.address }
    return 'localhost'
}
function Get-MasterAccessUrl([string]$Binding,[string]$LocalAddress) {
    if ($Binding.StartsWith(':')) { $Binding = '0.0.0.0'+$Binding }
    $uri = [Uri]('http://'+$Binding)
    $address = $uri.Host.Trim([char[]]'[]')
    if ($address -in @('0.0.0.0','::')) { $address = $LocalAddress }
    if (-not $address -or $address -in @('0.0.0.0','::')) { $address = 'localhost' }
    if ($address.Contains(':')) { $address = '['+$address+']' }
    return 'http://'+$address+':'+$uri.Port
}
function Show-MasterAccess {
    if ($Role -ne 'master') { return }
    $settings = Get-Content -LiteralPath $config -Raw | ConvertFrom-Json
    $binding = '0.0.0.0:1685'
    if ($settings.PSObject.Properties['listen'] -and $settings.listen) { $binding = [string]$settings.listen }
    $url = Get-MasterAccessUrl $binding (Get-LocalAccessAddress)
    Write-Host ((Get-InstallText 'Web console: {0}' '访问地址：{0}') -f $url) -ForegroundColor Cyan
}
function Show-InstallationSummary {
    Write-Host ((Get-InstallText "`nInstallation successful: {0} {1}" "`n安装成功：{0} {1}") -f $Role,$version) -ForegroundColor Green
    Show-MasterAccess
    Write-Host ((Get-InstallText 'Service: {0}' '服务：{0}') -f $ServiceName)
    Write-Host ('Installation: '+$InstallDir)
    Write-Host ((Get-InstallText 'Configuration: {0}' '配置文件：{0}') -f $config)
    Write-Host ((Get-InstallText 'Data: {0}' '数据目录：{0}') -f $DataDir)
    Write-Host ('CLI: '+$CliPath)
    Write-Host ("Tasks: & '"+$CliPath.Replace("'","''")+"' task list")
    Write-Host ((Get-InstallText 'Log: {0}' '日志：{0}') -f $script:logFile)
}
try {
    if ($ReleaseUrl) {
        $ReleaseUrl = $ReleaseUrl.TrimEnd('/')
        $uri = [Uri]$ReleaseUrl
        $masterPackages = $Role -eq 'agent' -and (Test-MasterPackageUrl $ReleaseUrl $MasterUrl)
        if (-not $uri.IsAbsoluteUri -or $uri.UserInfo -or $uri.Query -or $uri.Fragment -or ($uri.Scheme -ne 'https' -and -not ($uri.Scheme -eq 'http' -and ($uri.IsLoopback -or $masterPackages))) -or $uri.AbsolutePath -notmatch '^/[A-Za-z0-9._~/-]+$') { throw 'Release URL must use HTTPS or the selected Master invitation.' }
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
    Prepare-MasterPackages
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
    if (-not $Yes -and (Read-Host ((Get-InstallText 'Install {0} {1}? [y/N]' '安装 {0} {1}？[y/N]') -f $Role,$version)) -notmatch '^[yY]$') { return }
    if ($existing -or $AdoptConfig -or $hadCli -or $hadCliSidecars -or $hadDataCLIConnections) {
        Prepare-BackupDirectory
        if ($existing) { Maintain-Backups 'prune' }
        Test-BackupSpace
    }
    foreach ($path in $directories) { Protect-Directory $path }
    if ($existing) {
        $backup = New-TemporaryBackup
        if ($hadBinary) { Copy-Item -LiteralPath $binary -Destination (Join-Path $backup 'binary.exe') }
        Copy-Item -LiteralPath $config -Destination (Join-Path $backup 'service.json')
        Copy-Item -LiteralPath $metadata -Destination (Join-Path $backup 'install.json')
        $oldService = Service-Record
        if ($oldService) {
            if ($oldService.StartName -ne 'LocalSystem') { throw 'Review the custom service account before upgrading.' }
            Write-UTF8 (Join-Path $backup 'service-record.json') ($oldService | Select-Object PathName,StartMode | ConvertTo-Json)
            $wasRunning = $oldService.State -ne 'Stopped'
        }
        if ($Role -eq 'agent') {
            Write-Host (Get-InstallText 'Protecting Agent tasks during the upgrade...' '正在保留 Agent 上的任务进度...')
            Invoke-Native $candidate @('--config',$config,'--upgrade-maintenance','begin','--upgrade-maintenance-file',(Join-Path $work 'agent-maintenance.json')) | Out-Null
            $agentMaintenanceStarted = $true
        }
        $changed = $true
        Write-Host ((Get-InstallText 'Stopping {0} before the database backup...' '正在停止 {0}，随后备份数据库...') -f $ServiceName)
        Stop-OwnedService
        Invoke-Native $candidate @('--config',(Join-Path $backup 'service.json'),'--backup-state',(Join-Path $backup 'database')) | Out-Null
        $hadDatabaseBackup = $true
    } elseif ($AdoptConfig) {
        $backup = New-TemporaryBackup
        Copy-Item -LiteralPath $freshConfig -Destination (Join-Path $backup 'service.json')
        if ($autoAdopt -and $hadBinary) { Copy-Item -LiteralPath $binary -Destination (Join-Path $backup 'binary.exe') }
        if ($autoAdopt -and $service) {
            $oldService = Service-Record
            $wasRunning = $oldService.State -ne 'Stopped'
            $changed = $true
            Stop-OwnedService
        }
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
    Write-Host ((Get-InstallText 'Installing {0} {1}...' '正在安装 {0} {1}...') -f $Role,$version)
    Copy-Item -LiteralPath $candidate -Destination ($binary+'.new')
    Move-Item -LiteralPath ($binary+'.new') -Destination $binary -Force
    [IO.Directory]::CreateDirectory((Split-Path $CliPath -Parent)) | Out-Null
    Copy-Item -LiteralPath $cliCandidate -Destination ($CliPath+'.new')
    Move-Item -LiteralPath ($CliPath+'.new') -Destination $CliPath -Force
    $cliChanged = $true
    if (-not $existing) { Copy-Item -LiteralPath $freshConfig -Destination $config }
    $installationRecord = @{role=$Role; serviceName=$ServiceName; config=$config; version=$version; cliPath=$CliPath; backupDir=$BackupDir; dataDir=$DataDir}
    if ($InstallDir.Equals((Join-Path $base 'bin'),[StringComparison]::OrdinalIgnoreCase) -and
        $ConfigDir.Equals((Join-Path $base 'config'),[StringComparison]::OrdinalIgnoreCase) -and
        $DataDir.Equals((Join-Path $base 'data'),[StringComparison]::OrdinalIgnoreCase)) {
        $installationRecord.rootDir = $RootDir
    }
    Write-UTF8 $metadata ($installationRecord | ConvertTo-Json)
    if (-not $existing -and -not $AdoptConfig -and $Role -eq 'agent') { $env:OSS_TRANSFER_JOIN_TOKEN = $env:TRANSFER_JOIN_TOKEN; Invoke-Native $binary @('--config',$config,'--master-url',$MasterUrl,'--enroll-only') | Out-Null }
    Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
    Remove-Item Env:OSS_TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
    $command = '"'+$binary+'" --config "'+$config+'"'
    if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
        $result = Invoke-CimMethod -InputObject (Service-Record) -MethodName Change -Arguments @{PathName=$command}
        if ($result.ReturnValue -ne 0) { throw 'Could not update the service command.' }
    } else { New-Service -Name $ServiceName -BinaryPathName $command -DisplayName "OSS Transfer $Role" -StartupType Automatic | Out-Null }
    Prepare-Start
    Write-Host ((Get-InstallText 'Starting {0}...' '正在启动 {0}...') -f $ServiceName)
    Start-Service -Name $ServiceName
    Wait-Ready
    Publish-MasterPackages
    if ($agentMaintenanceStarted) {
        try {
            Invoke-Native $candidate @('--config',$config,'--upgrade-maintenance','end','--upgrade-maintenance-file',(Join-Path $work 'agent-maintenance.json')) | Out-Null
            $agentMaintenanceStarted = $false
        } catch { Write-Warning 'Could not end Agent upgrade maintenance; it expires automatically within 30 minutes.' }
    }
    Show-InstallationSummary
    if (-not ($Role -eq 'master' -and $WithAgent)) { Show-InitialCredentials }
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
        Write-Warning (Get-InstallText 'Installation failed; restoring the previous installation.' '安装失败，正在恢复原有安装。')
        try {
            Stop-OwnedService
            if ($cliChanged) {
                if ($hadCli) { Copy-Item -LiteralPath (Join-Path $backup 'cli.exe') -Destination $CliPath -Force }
                else { Remove-Item -LiteralPath $CliPath -Force }
                Restore-CLIConnections
            }
            if ($existing) {
                if ($hadDatabaseBackup) { Invoke-Native $candidate @('--config',(Join-Path $backup 'service.json'),'--restore-state',(Join-Path $backup 'database')) | Out-Null }
                if ($hadBinary) { Copy-Item -LiteralPath (Join-Path $backup 'binary.exe') -Destination $binary -Force }
                else { Remove-Item -LiteralPath $binary -Force -ErrorAction SilentlyContinue }
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
                if ($created -and -not $oldService) { Invoke-CimMethod -InputObject $created -MethodName Delete | Out-Null }
                if ($AdoptConfig -and $hadDatabaseBackup) {
                    Invoke-Native $candidate @('--config',$freshConfig,'--restore-state',(Join-Path $backup 'database')) | Out-Null
                    Write-Warning "Restored the adopted database. Backup: $backup"
                }
                if ($autoAdopt) {
                    Copy-Item -LiteralPath (Join-Path $backup 'service.json') -Destination $config -Force
                    if ($hadBinary) { Copy-Item -LiteralPath (Join-Path $backup 'binary.exe') -Destination $binary -Force }
                    else { Remove-Item -LiteralPath $binary -Force -ErrorAction SilentlyContinue }
                    Remove-Item -LiteralPath $metadata -Force -ErrorAction SilentlyContinue
                    if ($wasRunning -and $hadBinary) { Prepare-Start; Start-Service -Name $ServiceName; Wait-Ready }
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

if ($installationSucceeded -and $Role -eq 'master' -and $WithAgent) {
    try {
        # The Master keeps running if the optional Agent step fails. Rerunning this
        # command reuses the existing Master and any enrolled Agent identity.
        $localMaster = Read-PublishedMasterOrigin $DataDir
        $agentRoot = Resolve-LocalAgentRoot $RootDir $rootExplicit $InstallDir (Join-Path $env:ProgramFiles 'OSS Transfer\master')
        $agentInstall = Join-Path $agentRoot 'agent\bin'
        $agentConfig = Join-Path $agentRoot 'agent\config\service.json'
        $agentData = Join-Path $agentRoot 'agent\data'
        if (-not $rootExplicit -and $agentRoot.Equals($RootDir,[StringComparison]::OrdinalIgnoreCase) -and -not (Test-Path -LiteralPath (Join-Path $agentInstall 'install.json'))) {
            $legacyAgent = Join-Path $env:ProgramFiles 'OSS Transfer\agent'
            if (Test-Path -LiteralPath (Join-Path $legacyAgent 'install.json')) { $agentInstall = $legacyAgent }
        }
        $agentMetadata = Join-Path $agentInstall 'install.json'
        $agentService = if ($ServiceName -eq 'OSSTransferMaster') { 'OSSTransferAgent' } else { $ServiceName+'-Agent' }
        if (Test-Path -LiteralPath $agentMetadata) {
            Assert-RegularFile $agentMetadata
            $agentRecord = Get-Content -LiteralPath $agentMetadata -Raw | ConvertFrom-Json
            if ($agentRecord.role -ne 'agent') { throw 'Existing local installation is not an Agent.' }
            $agentConfig = $agentRecord.config
            $agentService = $agentRecord.serviceName
        }
        $retainedAgent = Test-Path -LiteralPath $agentConfig
        if ($retainedAgent) {
            Assert-RegularFile $agentConfig
            $agentSettings = Get-Content -LiteralPath $agentConfig -Raw | ConvertFrom-Json
            $agentData = $agentSettings.dataDirectory
            $agentService = $agentSettings.serviceName
            $grantPath = Join-Path $agentData 'cli-local-auth.json'
            Assert-RegularFile $grantPath
            $grant = Get-Content -LiteralPath $grantPath -Raw | ConvertFrom-Json
            try {
                $membership = Invoke-RestMethod -UseBasicParsing -Uri ($localMaster+'/api/auth/current') -Headers @{Authorization=('Bearer '+$grant.token)} -TimeoutSec 10
                if (-not $membership.success -or $membership.user.username -notlike 'local:agent:*') { throw 'Agent identity not recognized.' }
            } catch { throw 'The existing local Agent could not be verified as a member of this Master. Its data was left unchanged.' }
        }
        $agentArguments = @{Role='agent'; RootDir=$agentRoot; InstallDir=$agentInstall; ConfigDir=(Split-Path $agentConfig -Parent); DataDir=$agentData; ServiceName=$agentService; MasterUrl=$localMaster; Lang=$Lang; Yes=$true; StartupTimeout=$StartupTimeout}
        $cached = Join-Path $DataDir 'packages'
        if ((Test-Path -LiteralPath (Join-Path $cached "oss-transfer-windows-$arch.zip")) -and (Test-Path -LiteralPath (Join-Path $cached 'manifest.tsv'))) { $agentArguments.PackageDir = $cached }
        elseif ($PackageDir) { $agentArguments.PackageDir = $PackageDir }
        else { $agentArguments.ReleaseUrl = $ReleaseUrl }
        try {
            if (-not $retainedAgent) {
                $savedAuthToken = $env:OSS_TRANSFER_AUTH_TOKEN
                try {
                    Remove-Item Env:OSS_TRANSFER_AUTH_TOKEN -ErrorAction SilentlyContinue
                    $invitation = (Invoke-Native $CliPath @('--host',$localMaster,'--json','agent','invite','--label',$env:COMPUTERNAME,'--master-url',$localMaster) | Out-String) | ConvertFrom-Json
                } finally {
                    if ($savedAuthToken) { $env:OSS_TRANSFER_AUTH_TOKEN = $savedAuthToken }
                }
                if (-not $invitation.success -or -not $invitation.data.inviteToken) { throw 'Could not invite the local Agent.' }
                $env:TRANSFER_JOIN_TOKEN = $invitation.data.inviteToken
            }
            Write-Host (Get-InstallText 'Installing the local Agent...' '正在安装本机 Agent...')
            & $PSCommandPath @agentArguments
            Write-Host (Get-InstallText "`nMaster and local Agent installed successfully." "`nMaster 和本机 Agent 安装成功。") -ForegroundColor Green
        } finally { Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue }
    } finally {
        Show-MasterAccess
        Show-InitialCredentials
    }
}

} finally {
    if ($sourceWork) { Remove-Item -LiteralPath $sourceWork -Recurse -Force -ErrorAction SilentlyContinue }
}
