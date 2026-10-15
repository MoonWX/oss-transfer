# Run the installer's download preflight with HTTP fixtures; no service or disk
# outside a temporary directory is changed. Compatible with Windows PowerShell 5.1.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$installer = Join-Path $PSScriptRoot '../../deploy/go/Install.ps1'
$tokens=$null; $parseErrors=$null
$ast = [Management.Automation.Language.Parser]::ParseFile($installer,[ref]$tokens,[ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
$names = @('Normalize-ReleaseVersion','Invoke-SourceRequest','Get-PublicIPCountry','Get-SourceMetadata','Resolve-DownloadSource','Get-RegularZipEntry','Read-ZipEntryText','Get-ZipEntryHash','New-GitHubPackage')
foreach ($definition in $ast.FindAll({param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $names},$true)) {
    . ([ScriptBlock]::Create($definition.Extent.Text))
}
$work = Join-Path ([IO.Path]::GetTempPath()) ('oss-source-test-'+[Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($work) | Out-Null
$script:responses = @{}
$script:requests = [Collections.Generic.List[object]]::new()
function Invoke-WebRequest {
    param($Uri,$UseBasicParsing,$TimeoutSec,$Headers,$OutFile,$PassThru)
    $script:requests.Add(@{Url=[string]$Uri; Timeout=$TimeoutSec})
    if (-not $script:responses.ContainsKey([string]$Uri)) { throw [Net.WebException]::new('Fixture connection failure.') }
    $fixture = $script:responses[[string]$Uri]
    if ($fixture -is [ScriptBlock]) { return & $fixture $OutFile }
    if ($OutFile) { [IO.File]::WriteAllBytes($OutFile,$fixture) }
    return [PSCustomObject]@{ Content=$fixture; BaseResponse=[PSCustomObject]@{ResponseUri=[Uri]$Uri} }
}
function Assert($Condition,[string]$Message) { if (-not $Condition) { throw $Message } }
function Reject([ScriptBlock]$Action,[string]$Expected) {
    try { & $Action } catch { if ($_.Exception.Message -notlike "*$Expected*") { throw }; return }
    throw "Expected rejection: $Expected"
}
$script:checks = 0
function Check([string]$Name,[ScriptBlock]$Action) {
    $script:responses = @{}; $script:requests.Clear()
    & $Action
    $script:checks++
    Write-Host "PASS $Name"
}
function Hash-Bytes([byte[]]$Bytes) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash($Bytes))).Replace('-','').ToLowerInvariant() } finally { $sha.Dispose() }
}
$utf8 = [Text.UTF8Encoding]::new($false)
$githubAPI = 'https://api.github.com/repos/MoonWX/oss-transfer/releases/latest'
$jdoss = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer'
$githubURL = 'https://github.com/MoonWX/oss-transfer/releases/download/v4.1.5/oss-transfer-windows-amd64.zip'
$traceURL = 'https://www.cloudflare.com/cdn-cgi/trace'
$countryURL = 'https://ipinfo.io/country'
$fullCommit = 'a' * 40
$jdManifest = "format`toss-transfer-go-v1`nversion`t4.1.5`ncommit`t$fullCommit`n"
function GitHub-JSON([string]$Hash = ('b' * 64),[long]$Size = 123) {
    return (@{tag_name='v4.1.5';draft=$false;assets=@(@{name='oss-transfer-windows-amd64.zip';browser_download_url=$githubURL;digest="sha256:$Hash";size=$Size})} | ConvertTo-Json -Depth 6 -Compress)
}
function Add-JDOSS {
    $script:responses["$jdoss/latest/manifest.tsv"]=$jdManifest
    $script:responses["$jdoss/v4.1.5/manifest.tsv"]=$jdManifest
}
function New-TestArchive([string]$Fault='') {
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $path = Join-Path $work ([Guid]::NewGuid().ToString('N')+'.zip')
    $files = @{}
    foreach ($role in @('master','agent','cli')) { $files["oss-transfer-$role.exe"]=$utf8.GetBytes("fixture-$role") }
    $platform = if ($Fault -eq 'platform') {'windows/arm64'} else {'windows/amd64'}
    $commit = if ($Fault -eq 'commit') {'short'} else {$fullCommit}
    $files['VERSION']=$utf8.GetBytes("Version: 4.1.5`nCommit: aaaaaaa`nFullCommit: $commit`nBuildTime: 2026-10-15T00:00:00Z`nPlatform: $platform`n")
    $sums = @($files.Keys | Sort-Object | ForEach-Object { (Hash-Bytes $files[$_])+'  '+$_ })
    if ($Fault -eq 'checksum-duplicate') { $sums += $sums[0] }
    $files['SHA256SUMS']=$utf8.GetBytes(($sums -join "`n")+"`n")
    if ($Fault -eq 'binary') { $files['oss-transfer-agent.exe']=$utf8.GetBytes('altered') }
    $archive = [IO.Compression.ZipFile]::Open($path,[IO.Compression.ZipArchiveMode]::Create)
    try {
        foreach ($name in $files.Keys) {
            if ($Fault -eq 'missing' -and $name -eq 'oss-transfer-cli.exe') { continue }
            $entry = $archive.CreateEntry("oss-transfer-windows-amd64/$name")
            if ($Fault -eq 'symlink' -and $name -eq 'VERSION') { $entry.ExternalAttributes = (0xA000 -shl 16) }
            $stream = $entry.Open()
            try { $stream.Write($files[$name],0,$files[$name].Length) } finally { $stream.Dispose() }
        }
        if ($Fault -eq 'duplicate') {
            $entry = $archive.CreateEntry('oss-transfer-windows-amd64/VERSION')
            $stream = $entry.Open()
            try { $stream.Write($files['VERSION'],0,$files['VERSION'].Length) } finally { $stream.Dispose() }
        }
    } finally { $archive.Dispose() }
    $bytes=[IO.File]::ReadAllBytes($path)
    $script:responses[$githubURL]=$bytes
    $script:responses[$githubAPI]=GitHub-JSON (Hash-Bytes $bytes) $bytes.Length
    return Resolve-DownloadSource 'github' 'latest' 'amd64'
}
try {
    Check 'version validation' {
        Assert ((Normalize-ReleaseVersion '4.1.5') -ceq 'v4.1.5') 'Version normalization failed.'
        foreach ($invalid in @('main','../v4.1.5','v4.1.5?x','V4.1.5',"v4.1.5`n")) { Reject { Normalize-ReleaseVersion $invalid } 'Version must' }
    }
    Check 'CN public IP selects OSS and pins latest' {
        $script:responses[$traceURL]="ip=203.0.113.1`nloc=CN`n"; Add-JDOSS
        $result=Resolve-DownloadSource 'auto' 'latest' 'amd64'
        Assert ($result.Source -ceq 'jdoss' -and $result.ReleaseUrl -ceq "$jdoss/v4.1.5") 'China source was not pinned.'
        Assert ($script:requests[0].Timeout -eq 3) 'Geolocation timeout differs.'
    }
    Check 'non-CN public IP selects GitHub' {
        $script:responses[$traceURL]="loc=JP`n"; $script:responses[$githubAPI]=GitHub-JSON
        Assert ((Resolve-DownloadSource 'auto' 'latest' 'amd64').Source -ceq 'github') 'Wrong source outside China.'
    }
    Check 'country lookup fallback is bounded and strict' {
        $script:responses[$traceURL]="loc=cn`n"; $script:responses[$countryURL]="CN`n"; Add-JDOSS
        Assert ((Resolve-DownloadSource 'auto' 'latest' 'amd64').Source -ceq 'jdoss') 'Country fallback failed.'
        Assert ($script:requests[0].Timeout -eq 3 -and $script:requests[1].Timeout -eq 3) 'Geolocation lookup exceeded timeout.'
    }
    Check 'unknown public-IP location defaults to GitHub' {
        $script:responses[$countryURL]='CN extra'; $script:responses[$githubAPI]=GitHub-JSON
        Assert ((Resolve-DownloadSource 'auto' 'latest' 'amd64').Source -ceq 'github') 'Unknown location did not default to GitHub.'
    }
    Check 'unknown trace country tries the secondary provider' {
        $script:responses[$traceURL]='loc=XX'; $script:responses[$countryURL]='CN'; Add-JDOSS
        Assert ((Resolve-DownloadSource 'auto' 'latest' 'amd64').Source -ceq 'jdoss') 'Unknown trace country skipped the secondary lookup.'
    }
    Check 'GitHub metadata types must match the API schema' {
        $script:responses[$githubAPI]=(GitHub-JSON).Replace('"draft":false','"draft":"false"')
        Reject { Resolve-DownloadSource 'github' 'latest' 'amd64' } 'release identity'
    }
    Check 'explicit source does not geolocate or fallback' {
        $script:responses[$githubAPI]=GitHub-JSON
        Resolve-DownloadSource 'github' 'latest' 'amd64' | Out-Null
        Assert ($script:requests.Count -eq 1 -and $script:requests[0].Url -ceq $githubAPI) 'Explicit source caused a location request.'
        $script:responses=@{}
        Reject { Resolve-DownloadSource 'github' 'latest' 'amd64' } 'Could not download'
        Assert ($script:requests.Count -eq 2) 'Explicit source fell back.'
    }
    Check 'only initial metadata network failure permits auto fallback' {
        $script:responses[$traceURL]='loc=US'; Add-JDOSS
        Assert ((Resolve-DownloadSource 'auto' 'latest' 'amd64').Source -ceq 'jdoss') 'Auto source failed to fallback.'
    }
    Check 'malformed metadata cannot switch sources' {
        $script:responses[$traceURL]='loc=US'; $script:responses[$githubAPI]='{}'; Add-JDOSS
        Reject { Resolve-DownloadSource 'auto' 'latest' 'amd64' } 'tag_name'
        Assert (@($script:requests | Where-Object {$_.Url.StartsWith($jdoss)}).Count -eq 0) 'Malformed response caused mirror fallback.'
    }
    Check 'changing latest manifest refuses without fallback' {
        $script:responses[$traceURL]='loc=CN'; Add-JDOSS
        $script:responses["$jdoss/v4.1.5/manifest.tsv"]=$jdManifest+'unexpected'
        Reject { Resolve-DownloadSource 'auto' 'latest' 'amd64' } 'manifests differ'
        Assert (@($script:requests | Where-Object {$_.Url -ceq $githubAPI}).Count -eq 0) 'Publication mismatch caused fallback.'
    }
    Check 'archive URL cannot leave the fixed repository and tag' {
        $script:responses[$githubAPI]=(GitHub-JSON).Replace('MoonWX/oss-transfer/releases/download','elsewhere/repository/releases/download')
        Reject { Resolve-DownloadSource 'github' 'latest' 'amd64' } 'archive URL'
    }
    Check 'GitHub requires an API checksum' {
        $script:responses[$githubAPI]=(GitHub-JSON).Replace('sha256:','')
        Reject { Resolve-DownloadSource 'github' 'latest' 'amd64' } 'checksum'
    }
    Check 'explicit version uses the matching tag endpoint' {
        $endpoint='https://api.github.com/repos/MoonWX/oss-transfer/releases/tags/v4.1.5'
        $script:responses[$endpoint]=GitHub-JSON
        $release=Resolve-DownloadSource 'github' 'v4.1.5' 'amd64'
        Assert ($release.Version -ceq 'v4.1.5' -and $script:requests.Count -eq 1) 'Explicit version was resolved more than once.'
    }
    Check 'duplicate GitHub platform assets refuse' {
        $metadata=(GitHub-JSON) | ConvertFrom-Json
        $metadata.assets=@($metadata.assets[0],$metadata.assets[0])
        $script:responses[$githubAPI]=$metadata | ConvertTo-Json -Depth 6 -Compress
        Reject { Resolve-DownloadSource 'github' 'latest' 'amd64' } 'duplicate GitHub platform archive'
    }
    Check 'insecure metadata redirect refuses without mirror fallback' {
        $script:responses[$traceURL]='loc=US'
        $script:responses[$githubAPI]={ [PSCustomObject]@{Content=(GitHub-JSON);BaseResponse=[PSCustomObject]@{ResponseUri=[Uri]'http://example.test/release'}} }
        Add-JDOSS
        Reject { Resolve-DownloadSource 'auto' 'latest' 'amd64' } 'insecure URL'
        Assert (@($script:requests | Where-Object {$_.Url.StartsWith($jdoss)}).Count -eq 0) 'Insecure redirect caused fallback.'
    }
    Check 'missing pinned OSS manifest refuses without mirror fallback' {
        $script:responses[$traceURL]='loc=CN'
        $script:responses["$jdoss/latest/manifest.tsv"]=$jdManifest
        $script:responses[$githubAPI]=GitHub-JSON
        Reject { Resolve-DownloadSource 'auto' 'latest' 'amd64' } 'verify the versioned release manifest'
        Assert (@($script:requests | Where-Object {$_.Url -ceq $githubAPI}).Count -eq 0) 'Pinned download failure caused fallback.'
    }
    Check 'archive validates and generates the existing installer manifest' {
        $release=New-TestArchive
        $package=Join-Path $work 'valid-package'
        New-GitHubPackage $release $package
        $manifest=Get-Content -LiteralPath (Join-Path $package 'manifest.tsv')
        Assert (@($manifest | Where-Object { $_.StartsWith("file`t"+"windows`t"+"amd64`t") }).Count -eq 3) 'Package roles missing.'
        Assert (@($manifest | Where-Object { $_ -ceq "commit`t$fullCommit" }).Count -eq 1) 'Manifest commit missing.'
        Assert (@(Get-ChildItem -LiteralPath $package).Count -eq 2) 'Preflight unpacked arbitrary members.'
    }
    Check 'outer checksum mismatch refuses before ZIP parsing' {
        $release=New-TestArchive; $release.Hash='0'*64
        Reject { New-GitHubPackage $release (Join-Path $work 'bad-outer') } 'Archive checksum mismatch'
    }
    foreach ($case in @(@('platform','version, commit or platform'),@('commit','version, commit or platform'),@('checksum-duplicate','Duplicate package checksum'),@('binary','binary checksum'),@('missing','must occur once'),@('symlink','regular archive member'),@('duplicate','must occur once'))) {
        Check "reject archive $($case[0])" {
            $release=New-TestArchive $case[0]
            Reject { New-GitHubPackage $release (Join-Path $work $case[0]) } $case[1]
        }
    }
    Write-Host "$script:checks source/preflight checks passed."
} finally { Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue }
