# Native Go service installation. Run from an elevated PowerShell session.
[CmdletBinding()]
param(
    [ValidateSet('master','agent')][string]$Role,
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
if ($env:OS -ne 'Windows_NT') { throw 'This installer requires Windows.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run PowerShell as Administrator.' }
if (-not $ReleaseUrl -and -not $PackageDir) { $ReleaseUrl = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest' }
if ($ReleaseUrl -and $PackageDir) { throw 'Choose -ReleaseUrl or -PackageDir.' }
if ($ReleaseUrl) { $ReleaseUrl = $ReleaseUrl.TrimEnd('/') }
if ($AdoptConfig -and $Upgrade) { throw 'Choose -AdoptConfig or -Upgrade.' }
$architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
$arch = switch ($architecture) { 'x64' {'amd64'} 'arm64' {'arm64'} default {throw 'Unsupported CPU architecture.'} }
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
            $childArguments.InstallDir = $directory
            $childArguments.Yes = $true
            & $PSCommandPath @childArguments
        }
        return
    }
    $recordPath = Join-Path $InstallDir 'install.json'
    if (-not (Test-Path -LiteralPath $recordPath -PathType Leaf) -or ((Get-Item -LiteralPath $recordPath -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'No managed Go installation found in the selected directory.' }
    $record = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json
    if ($record.role -notin @('master','agent') -or ($Role -and $Role -ne $record.role)) { throw 'Installed role differs from -Role.' }
    $Role = $record.role
    if (-not $ServiceName) { $ServiceName = $record.serviceName }
    if (-not $ConfigDir) { $ConfigDir = Split-Path $record.config -Parent }
    if (-not $CliPath -and $record.PSObject.Properties['cliPath']) { $CliPath = $record.cliPath }
    if (-not $BackupDir -and $record.PSObject.Properties['backupDir']) { $BackupDir = $record.backupDir }
    if (-not $DataDir) {
        if (-not (Test-Path -LiteralPath $record.config -PathType Leaf) -or ((Get-Item -LiteralPath $record.config -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Installed configuration is missing or linked.' }
        $savedConfig = Get-Content -LiteralPath $record.config -Raw | ConvertFrom-Json
        $DataDir = $savedConfig.dataDirectory
        if (-not $DataDir) { throw 'Installed data directory is missing.' }
    }
}
if (-not $Role) { $Role = 'master' }
if (-not $ServiceName) { $ServiceName = "OSSTransferGo$((Get-Culture).TextInfo.ToTitleCase($Role))" }
if ($ServiceName -notmatch '^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$') { throw 'Invalid service name.' }
$base = Join-Path $env:ProgramData "OSS Transfer\$Role"
if (-not $InstallDir) { $InstallDir = Join-Path $env:ProgramFiles "OSS Transfer\$Role" }
if (-not $ConfigDir) { $ConfigDir = Join-Path $base 'config' }
if (-not $DataDir) { $DataDir = Join-Path $base 'data' }
if (-not $BackupDir) { $BackupDir = Join-Path $base 'backups' }
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
$cliChanged = $false
$database = Join-Path $DataDir "$Role.db"
$existing = Test-Path -LiteralPath $metadata
$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existing) {
    if (-not $Upgrade) { throw 'Go installation exists; rerun with -Upgrade.' }
    Assert-RegularFile $metadata; Assert-RegularFile $binary; Assert-RegularFile $config
    $installed = Get-Content -LiteralPath $metadata -Raw | ConvertFrom-Json
    if ($installed.role -ne $Role -or $installed.serviceName -ne $ServiceName -or $installed.config -ne $config) { throw 'Existing installation identity does not match.' }
} else {
    if ($Upgrade) { throw 'No managed Go installation found; this does not upgrade Java V4.0.x.' }
    if ($service) { throw 'An unmanaged service already exists.' }
    foreach ($path in @($InstallDir,$ConfigDir,$DataDir)) {
        if ($AdoptConfig -and $path -eq $DataDir) { continue }
        if ((Test-Path -LiteralPath $path) -and @(Get-ChildItem -LiteralPath $path -Force).Count -gt 0) { throw "Refusing nonempty directory: $path" }
    }
}
if ($existing -and $installed.PSObject.Properties['cliPath'] -and $installed.cliPath -ne $CliPath) { throw 'CLI path differs from the installed path; keep -CliPath unchanged during upgrade.' }
if ($hadCli -and (-not $existing -or -not $installed.PSObject.Properties['cliPath'] -or $installed.cliPath -ne $CliPath) -and -not $ReplaceCli) { throw "CLI already exists: $CliPath; use -ReplaceCli to back it up and replace it, or choose -CliPath." }
$work = Join-Path ([IO.Path]::GetTempPath()) ('oss-transfer-install-'+[Guid]::NewGuid().ToString('N'))
Protect-Directory $work
$changed = $false
$backup = $null
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
    $output = & $Executable @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Native command failed (exit $LASTEXITCODE)." }
    return $output
}
function Service-Record { Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue }
function Stop-OwnedService {
    $current = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($current -and $current.Status -ne 'Stopped') { Stop-Service -Name $ServiceName; $current.WaitForStatus('Stopped',[TimeSpan]::FromSeconds(50)) }
}
function Prepare-Start {
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
        Assert-RegularFile $database
        if ((Get-Item -LiteralPath $database).Length -eq 0) { throw 'Existing Go database is empty; refusing to initialize a replacement.' }
        if ($AdoptConfig) {
            $freshConfig = Join-Path $work 'service.json'
            Copy-Item -LiteralPath $sourceConfig -Destination $freshConfig
        }
    } else {
        if ($Role -eq 'agent' -and (-not $MasterUrl -or -not $env:TRANSFER_JOIN_TOKEN)) { throw 'First Agent installation requires -MasterUrl and TRANSFER_JOIN_TOKEN.' }
        $script:readyFile = Join-Path $DataDir 'runtime\ready'
        $settings = @{ serviceName=$ServiceName; dataDirectory=$DataDir; readyFile=$script:readyFile; logFile=(Join-Path $DataDir 'logs\service.log') }
        if ($Role -eq 'master') { $settings.listen = $Listen }
        $freshConfig = Join-Path $work 'service.json'
        Write-UTF8 $freshConfig ($settings | ConvertTo-Json)
        Invoke-Native $candidate @('--config',$freshConfig,'--check-config') | Out-Null
    }
    if ((Test-Path (Join-Path $DataDir 'CURRENT')) -or (Test-Path (Join-Path $DataDir 'rocksdb\CURRENT'))) { throw 'RocksDB requires an explicit migration.' }
    if (-not $Yes -and (Read-Host "Install $Role $version? [y/N]") -notmatch '^[yY]$') { return }
    if ($AdoptConfig) { Invoke-Native $candidate @('--config',$freshConfig,'--backup-state',(Join-Path $work 'database')) | Out-Null }
    foreach ($path in $directories) { Protect-Directory $path }
    if ($existing) {
        $backup = Join-Path $BackupDir ($ServiceName+'-'+[DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')+'-'+[Guid]::NewGuid().ToString('N'))
        Protect-Directory $backup
        Write-Host "Backup: $backup"
        Copy-Item -LiteralPath $binary -Destination (Join-Path $backup 'binary.exe')
        Copy-Item -LiteralPath $config -Destination (Join-Path $backup 'service.json')
        Copy-Item -LiteralPath $metadata -Destination (Join-Path $backup 'install.json')
        $oldService = Service-Record
        if ($oldService) {
            if ($oldService.StartName -ne 'LocalSystem') { throw 'Review the custom service account before upgrading.' }
            Write-UTF8 (Join-Path $backup 'service-record.json') ($oldService | Select-Object PathName,StartMode | ConvertTo-Json)
            $wasRunning = $oldService.State -ne 'Stopped'
        }
        $changed = $true
        Stop-OwnedService
        Invoke-Native $candidate @('--config',(Join-Path $backup 'service.json'),'--backup-state',(Join-Path $backup 'database')) | Out-Null
        $hadDatabaseBackup = $true
    } elseif ($AdoptConfig) {
        $backup = Join-Path $BackupDir ($ServiceName+'-'+[DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')+'-'+[Guid]::NewGuid().ToString('N'))
        Protect-Directory $backup
        Copy-Item -LiteralPath (Join-Path $work 'database') -Destination (Join-Path $backup 'database')
        Copy-Item -LiteralPath $freshConfig -Destination (Join-Path $backup 'service.json')
        $hadDatabaseBackup = $true
        Write-Host "Backup: $backup"
    }
    if ($hadCli) {
        if (-not $backup) {
            $backup = Join-Path $BackupDir ($ServiceName+'-'+[DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')+'-'+[Guid]::NewGuid().ToString('N'))
            Protect-Directory $backup
            Write-Host "Backup: $backup"
        }
        Copy-Item -LiteralPath $CliPath -Destination (Join-Path $backup 'cli.exe')
    }
    $changed = $true
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
    } else { New-Service -Name $ServiceName -BinaryPathName $command -DisplayName "OSS Transfer Go $Role" -StartupType Automatic | Out-Null }
    Prepare-Start
    Start-Service -Name $ServiceName
    Wait-Ready
    Write-Host "Installed $Role $version. Service: $ServiceName"
    Write-Host "Configuration: $config"
    Write-Host "CLI: $CliPath"
    if ($Role -eq 'master') {
        $installedListen = if ($existing -or $AdoptConfig) { $fields.listen } else { $Listen }
        $cliListen = $installedListen -replace '^0\.0\.0\.0:', '127.0.0.1:' -replace '^\[::\]:', '[::1]:' -replace '^:', '127.0.0.1:'
        Write-Host ("Login: & '"+$CliPath.Replace("'","''")+"' --host 'http://"+$cliListen+"' login")
    }
    Write-Host ('Log: '+(Join-Path $DataDir 'logs\service.log'))
    if (-not $existing -and -not $AdoptConfig -and $Role -eq 'master') { Write-Host 'Open the Master URL to finish setup. The setup token is in the service log.' }
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
                if ($wasRunning) { Prepare-Start; Start-Service -Name $ServiceName; Wait-Ready }
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
    throw $failure
} finally {
    Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
    Remove-Item Env:OSS_TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
