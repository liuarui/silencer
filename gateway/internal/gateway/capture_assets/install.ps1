param([switch]$Uninstall)
$ErrorActionPreference = 'Stop'
$CaptureRoot = if ($env:SILENCER_DATA_DIR) { $env:SILENCER_DATA_DIR } else { Join-Path $env:LOCALAPPDATA 'Silencer' }
$TaskName = 'Silencer Local'
$TaskPath = '\'
$JobSucceeded = $false
$ChangeStarted = $false
$BackupRoot = $null
$ExtractPath = $null
$PreviousTaskXml = $null
$RollbackComplete = $true
$ReplacedVersionPath = $null
function Assert-WindowsPlatform {
    if ($env:OS -ne 'Windows_NT' -or ($env:PROCESSOR_ARCHITECTURE -ne 'AMD64' -and $env:PROCESSOR_ARCHITEW6432 -ne 'AMD64') -or [Environment]::OSVersion.Version.Major -lt 10) { throw 'Windows 10/11 x64 is required' }
}
function Get-InstallerIdentity { [Security.Principal.WindowsIdentity]::GetCurrent() }
function Get-ManagedTask {
    @(Get-ScheduledTask -ErrorAction Stop | Where-Object { $_.TaskName -eq $TaskName -and $_.TaskPath -eq $TaskPath })
}
function Assert-ManagedTaskOwnership {
    $Tasks = @(Get-ManagedTask)
    if ($Tasks.Count -gt 1) { throw 'Ambiguous background task registration' }
    $ExpectedArguments = '-NoProfile -WindowStyle Hidden -ExecutionPolicy RemoteSigned -File "' + (Join-Path $CaptureRoot 'launch.ps1') + '"'
    foreach ($Task in $Tasks) {
        if ($Task.Actions.Count -ne 1 -or $Task.Actions[0].Arguments -ne $ExpectedArguments -or [IO.Path]::GetFileName($Task.Actions[0].Execute) -ne 'powershell.exe') {
            throw 'Service belongs to another installation; no service was changed.'
        }
    }
}
function Protect-InstallationDirectory {
    New-Item -ItemType Directory -Force -Path $CaptureRoot | Out-Null
    Protect-InstallationItem $CaptureRoot
}
function Protect-InstallationItem([string]$Path) {
    $Item = Get-Item -LiteralPath $Path -Force
    if ($Item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Installation must not contain reparse points' }
    $Identity = (Get-InstallerIdentity).User
    $Acl = if ($Item.PSIsContainer) { [Security.AccessControl.DirectorySecurity]::new() } else { [Security.AccessControl.FileSecurity]::new() }
    $Acl.SetOwner($Identity)
    $Acl.SetAccessRuleProtection($true, $false)
    foreach ($Sid in @($Identity, [Security.Principal.SecurityIdentifier]::new('S-1-5-18'))) {
        $Inheritance = if ($Item.PSIsContainer) { 'ContainerInherit,ObjectInherit' } else { 'None' }
        $Rule = [Security.AccessControl.FileSystemAccessRule]::new($Sid, 'FullControl', $Inheritance, 'None', 'Allow')
        $Acl.AddAccessRule($Rule)
    }
    Set-Acl -LiteralPath $Path -AclObject $Acl
    if ($Item.PSIsContainer) {
        foreach ($Child in Get-ChildItem -LiteralPath $Path -Force) { Protect-InstallationItem $Child.FullName }
    }
}
function Stop-ManagedGateway {
    Assert-ManagedTaskOwnership
    if (@(Get-ManagedTask).Count) { Stop-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -ErrorAction Stop }
    $Processes = @(Get-CimInstance Win32_Process -Filter "Name='silencer-gateway.exe' OR Name='mitmdump.exe'" | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith((Join-Path $CaptureRoot 'versions') + '\', [StringComparison]::OrdinalIgnoreCase) })
    foreach ($Process in $Processes) {
        Stop-Process -Id $Process.ProcessId -ErrorAction SilentlyContinue
        Wait-Process -Id $Process.ProcessId -Timeout 15 -ErrorAction SilentlyContinue
        if (Get-Process -Id $Process.ProcessId -ErrorAction SilentlyContinue) { throw 'Old gateway did not stop' }
    }
}
function Test-NativeBundle([string]$Folder) {
    return (Test-Path (Join-Path $Folder 'silencer-gateway.exe')) -and (Test-Path (Join-Path $Folder 'runtime\mitmdump.exe')) -and (Test-Path (Join-Path $Folder 'web_dist\index.html'))
}
function Restore-ManagedInstallation {
    Stop-ManagedGateway
    if ($ReplacedVersionPath) {
        if (Test-Path -LiteralPath $ReplacedVersionPath) { Remove-Item -LiteralPath $ReplacedVersionPath -Recurse -Force }
        Move-Item -LiteralPath (Join-Path $BackupRoot 'replaced-version') -Destination $ReplacedVersionPath
    }
    foreach ($Name in @('current','launch.ps1','release-base','.silencer-local-install')) {
        $Saved = Join-Path $BackupRoot $Name
        $Destination = Join-Path $CaptureRoot $Name
        if (Test-Path $Saved) { Copy-Item -Force $Saved $Destination } else { Remove-Item $Destination -ErrorAction SilentlyContinue }
    }
    if ($PreviousTaskXml) {
        Assert-ManagedTaskOwnership
        Register-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -Xml $PreviousTaskXml -Force | Out-Null
        Start-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath
    } elseif (@(Get-ManagedTask).Count) { Unregister-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -Confirm:$false -ErrorAction Stop }
}
function Get-HttpsFile([string]$Url, [string]$Destination) {
    for ($Hop = 0; $Hop -lt 8; $Hop++) {
        $Uri = [Uri]$Url
        if ($Uri.Scheme -ne 'https' -or $Uri.UserInfo) { throw 'HTTPS downloads without URL credentials are required' }
        $Request = [Net.HttpWebRequest]::Create($Uri)
        $Request.AllowAutoRedirect = $false
        $Response = $Request.GetResponse()
        try {
            if ([int]$Response.StatusCode -ge 300 -and [int]$Response.StatusCode -lt 400) {
                $Url = ([Uri]::new($Uri, $Response.Headers['Location'])).AbsoluteUri
                continue
            }
            $File = [IO.File]::Create($Destination)
            try { $Response.GetResponseStream().CopyTo($File) } finally { $File.Dispose() }
            return
        } finally { $Response.Dispose() }
    }
    throw 'Too many download redirects'
}
try {
Assert-ManagedTaskOwnership
if ($env:SILENCER_SETUP_JOB_ID -match '^[a-f0-9]{32}$' -and $env:SILENCER_SETUP_JOB_FILE) {
    if ($env:SILENCER_SETUP_JOB_ACTION -notin @('repair','update','uninstall')) { throw 'Invalid maintenance action' }
    $CreatedAt = if ($env:SILENCER_SETUP_JOB_CREATED_AT) { $env:SILENCER_SETUP_JOB_CREATED_AT } else { [DateTime]::UtcNow.ToString('o') }
    $StartingJob = @{ id=$env:SILENCER_SETUP_JOB_ID; action=$env:SILENCER_SETUP_JOB_ACTION; state='running'; pid=$PID; created_at=$CreatedAt }
    $StartingFile = $env:SILENCER_SETUP_JOB_FILE + '.installer-start'
    [IO.File]::WriteAllText($StartingFile, ($StartingJob | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
    Move-Item -Force $StartingFile $env:SILENCER_SETUP_JOB_FILE
    Start-Sleep -Seconds 1
}
if ($Uninstall) {
    $Marker = Join-Path $CaptureRoot '.silencer-local-install'
    if (-not (Test-Path $Marker) -or (Get-Content $Marker -Raw).Trim() -ne 'windows') { throw 'Not a managed Silencer installation' }
    # This script is detached before the task is stopped, so cleanup survives it.
    Stop-ManagedGateway
    $RegisteredTasks = @(Get-ScheduledTask -ErrorAction Stop | Where-Object { $_.TaskName -eq $TaskName -and $_.TaskPath -eq '\' })
    if ($RegisteredTasks.Count) { Unregister-ScheduledTask -TaskName $TaskName -TaskPath '\' -Confirm:$false -ErrorAction Stop }
    if (@(Get-ScheduledTask -ErrorAction Stop | Where-Object { $_.TaskName -eq $TaskName -and $_.TaskPath -eq '\' }).Count) { throw 'Background task remains registered' }
    foreach ($Name in @('current', 'launch.ps1')) {
        $File = Join-Path $CaptureRoot $Name
        if (Test-Path $File) { Remove-Item $File -ErrorAction Stop }
    }
    # Preserve the marker until all service/file operations have succeeded, so
    # an interrupted or denied removal can be retried by the same script.
    Remove-Item -LiteralPath $Marker -Force -ErrorAction Stop
    Write-Output 'Service removed; certificates, settings, logs and mappings retained.'
    $JobSucceeded = $true
    return
}
Assert-WindowsPlatform
Protect-InstallationDirectory
$ReleaseBase = $env:SILENCER_RELEASE_BASE
if (-not $ReleaseBase -and (Test-Path (Join-Path $CaptureRoot 'release-base'))) { $ReleaseBase = (Get-Content (Join-Path $CaptureRoot 'release-base') -Raw).Trim() }
if (-not $ReleaseBase) { $ReleaseBase = 'https://github.com/liuarui/silencer/releases/latest/download' }
if (-not $ReleaseBase.StartsWith('https://')) { throw 'Set SILENCER_RELEASE_BASE to your HTTPS release URL.' }
New-Item -ItemType Directory -Force -Path (Join-Path $CaptureRoot 'versions') | Out-Null
$ManifestFile = Join-Path $CaptureRoot 'manifest.download.json'
Get-HttpsFile "$ReleaseBase/manifest.json" $ManifestFile
$Manifest = Get-Content $ManifestFile -Raw | ConvertFrom-Json
Remove-Item $ManifestFile
$Entry = $Manifest.platforms.'windows-amd64'
if (-not $Entry -or [IO.Path]::GetFileName($Entry.file) -ne $Entry.file -or $Entry.sha256 -notmatch '^[a-f0-9]{64}$') { throw 'Unsupported platform or invalid release manifest' }
$Archive = Join-Path $CaptureRoot 'bundle.zip'
Get-HttpsFile "$ReleaseBase/$($Entry.file)" $Archive
if ((Get-FileHash $Archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $Entry.sha256) { throw 'Artifact checksum mismatch' }
$VersionPath = Join-Path (Join-Path $CaptureRoot 'versions') $Entry.sha256
$ExtractPath = Join-Path (Join-Path $CaptureRoot 'versions') ('.download.' + [Guid]::NewGuid().ToString('N'))
Expand-Archive $Archive $ExtractPath
Remove-Item $Archive
if (-not (Test-NativeBundle $ExtractPath)) { throw 'Incomplete native bundle' }
$PreviousTask = @(Get-ManagedTask) | Select-Object -First 1
if ($PreviousTask -and -not (Test-Path (Join-Path $CaptureRoot '.silencer-local-install'))) { throw 'Existing unmanaged task conflicts with Silencer' }
if ($PreviousTask) { $PreviousTaskXml = Export-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath }
$BackupRoot = Join-Path $CaptureRoot ('.rollback.' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $BackupRoot | Out-Null
foreach ($Name in @('current','launch.ps1','release-base','.silencer-local-install')) {
    $Original = Join-Path $CaptureRoot $Name
    if (Test-Path $Original) { Copy-Item $Original (Join-Path $BackupRoot $Name) }
}
$ChangeStarted = $true
Stop-ManagedGateway
if (Test-Path -LiteralPath $VersionPath) {
    Move-Item -LiteralPath $VersionPath -Destination (Join-Path $BackupRoot 'replaced-version')
    $ReplacedVersionPath = $VersionPath
}
# A checksum authenticates the downloaded archive, not an old extracted cache.
# Reinstall the verified files even when damaged files still exist by name.
Move-Item -LiteralPath $ExtractPath -Destination $VersionPath
$ExtractPath = $null
$Entry.sha256 | Set-Content (Join-Path $CaptureRoot 'current.new') -Encoding ASCII
Move-Item -Force (Join-Path $CaptureRoot 'current.new') (Join-Path $CaptureRoot 'current')
$ReleaseBase | Set-Content (Join-Path $CaptureRoot 'release-base') -Encoding ASCII
$Launcher = Join-Path $CaptureRoot 'launch.ps1'
@'
$ErrorActionPreference = 'Stop'
$CaptureRoot = $PSScriptRoot
$Version = (Get-Content (Join-Path $CaptureRoot 'current') -Raw).Trim()
if ($Version -notmatch '^[a-f0-9]{64}$') { throw 'Invalid installed version' }
$Bundle = Join-Path (Join-Path $CaptureRoot 'versions') $Version
$env:SILENCER_LOCAL_MODE = 'true'
$env:SILENCER_INSTALLATION_ID = $Version
$env:SILENCER_GATEWAY_ADDR = '127.0.0.1:18711'
$env:SILENCER_CONTROL_CONFIG_FILE = Join-Path $CaptureRoot 'control.json'
$env:SILENCER_WEB_DIST = Join-Path $Bundle 'web_dist'
$env:SILENCER_SQLITE_PATH = Join-Path $CaptureRoot 'silencer.db'
$env:SILENCER_MITMDUMP_PATH = Join-Path $Bundle 'runtime\mitmdump.exe'
$env:PATH = (Join-Path $Bundle 'lib') + ';' + $env:PATH
& (Join-Path $Bundle 'silencer-gateway.exe')
exit $LASTEXITCODE
'@ | Set-Content $Launcher -Encoding UTF8
$Action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -WindowStyle Hidden -ExecutionPolicy RemoteSigned -File `"$Launcher`""
$Trigger = New-ScheduledTaskTrigger -AtLogOn -User (Get-InstallerIdentity).Name
$Settings = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)
Assert-ManagedTaskOwnership
Register-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -Action $Action -Trigger $Trigger -Settings $Settings -Force | Out-Null
Start-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath
'windows' | Set-Content (Join-Path $CaptureRoot '.silencer-local-install') -Encoding ASCII
$SetupMode = if ($env:SILENCER_SETUP_MODE -eq 'remote') { 'remote' } else { 'local' }
$Ready = $false
for ($Attempt = 0; $Attempt -lt 30; $Attempt++) {
    try { $Identity = Invoke-RestMethod 'http://127.0.0.1:18711/local-readyz' -TimeoutSec 1; if ($Identity -eq $Entry.sha256) { $Ready = $true; break } } catch { }
    Start-Sleep -Seconds 1
}
if (-not $Ready) { throw 'Local gateway failed to become ready' }
$JobSucceeded = $true
try { Start-Process "http://127.0.0.1:18711/#/capture?mode=$SetupMode&resume=1" } catch { Write-Output "http://127.0.0.1:18711/#/capture?mode=$SetupMode&resume=1" }
} finally {
    if ($ChangeStarted -and -not $JobSucceeded) {
        $RollbackComplete = $false
        try { Restore-ManagedInstallation; $RollbackComplete = $true } catch { Write-Warning "Installation rollback failed; recovery files retained at $BackupRoot" }
    }
    if ($ExtractPath) { Remove-Item -Recurse -Force $ExtractPath -ErrorAction SilentlyContinue }
    if ($BackupRoot -and $RollbackComplete) { Remove-Item -Recurse -Force $BackupRoot -ErrorAction SilentlyContinue }
    if ($env:SILENCER_SETUP_JOB_ID -match '^[a-f0-9]{32}$' -and $env:SILENCER_SETUP_JOB_FILE) {
        $JobState = if ($JobSucceeded) { 'complete' } else { 'failed' }
        $Job = @{ id=$env:SILENCER_SETUP_JOB_ID; action=$env:SILENCER_SETUP_JOB_ACTION; state=$JobState }
        if (-not $JobSucceeded) { $Job.error = 'maintenance_failed' }
        $JobTemporary = $env:SILENCER_SETUP_JOB_FILE + '.installer-new'
        [IO.File]::WriteAllText($JobTemporary, ($Job | ConvertTo-Json -Compress), [Text.UTF8Encoding]::new($false))
        Move-Item -Force $JobTemporary $env:SILENCER_SETUP_JOB_FILE
    }
}
