# Isolated Windows installer behavior tests. Native service, process, network,
# ACL and browser operations are replaced before executing the reviewed AST.
# No HOME changes, scheduled tasks, certificate stores or real processes.
$ErrorActionPreference = 'Stop'
$Source = Join-Path $PSScriptRoot '../gateway/internal/gateway/capture_assets/install.ps1'
$Tokens = $null; $ParseErrors = $null
$Ast = [System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path $Source), [ref]$Tokens, [ref]$ParseErrors)
if ($ParseErrors) { throw 'Installer syntax errors' }
foreach ($Name in @('install.ps1', 'uninstall.ps1')) {
    $Other = Join-Path (Split-Path $Source) $Name
    [System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path $Other), [ref]$Tokens, [ref]$ParseErrors) | Out-Null
    if ($ParseErrors) { throw "$Name syntax errors" }
}
$Statements = $Ast.EndBlock.Statements
$Initialize = ($Statements | Where-Object { $_ -is [System.Management.Automation.Language.AssignmentStatementAst] } | ForEach-Object { $_.Extent.Text }) -join "`n"
$Main = ($Statements | Where-Object { $_ -is [System.Management.Automation.Language.TryStatementAst] } | Select-Object -Last 1).Extent.Text
foreach ($Definition in $Statements | Where-Object { $_ -is [System.Management.Automation.Language.FunctionDefinitionAst] }) {
    . ([ScriptBlock]::Create($Definition.Extent.Text))
}
$Temporary = Join-Path ([IO.Path]::GetTempPath()) ('silencer-windows-tests-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $Temporary | Out-Null
$SavedEnvironment = @{}
foreach ($Item in Get-ChildItem Env: | Where-Object { $_.Name.StartsWith('SILENCER_') }) {
    $SavedEnvironment[$Item.Name] = $Item.Value
    [Environment]::SetEnvironmentVariable($Item.Name, $null, 'Process')
}
$script:Fixture = @{ Data = (Join-Path $Temporary 'data'); Tasks = @(); Calls = @(); Ready = $true; DenyUnregister = $false; FailEnumeration = $false }

# Every potentially consequential operation is a local fixture function.
function Assert-WindowsPlatform { }
function Get-InstallerIdentity { [PSCustomObject]@{ Name='fixture-user' } }
function Protect-InstallationDirectory { New-Item -ItemType Directory -Force -Path $CaptureRoot | Out-Null }
function Get-ScheduledTask {
    param($ErrorAction)
    if ($script:Fixture.FailEnumeration) { throw 'Fixture cannot enumerate tasks' }
    $script:Fixture.Tasks
}
function Stop-ScheduledTask { param($TaskName, $TaskPath, $ErrorAction); $script:Fixture.Calls += "stop:$TaskPath" }
function Start-ScheduledTask { param($TaskName, $TaskPath); $script:Fixture.Calls += "start:$TaskPath" }
function Unregister-ScheduledTask {
    param($TaskName, $TaskPath, [switch]$Confirm, $ErrorAction)
    if ($script:Fixture.DenyUnregister) { throw 'Fixture unregister denied' }
    $script:Fixture.Calls += "remove:$TaskPath"; $script:Fixture.Tasks = @()
}
function Export-ScheduledTask { param($TaskName, $TaskPath); ConvertTo-Json -InputObject $script:Fixture.Tasks[0] -Depth 8 -Compress }
function Register-ScheduledTask {
    param($TaskName, $TaskPath, $Xml, $Action, $Trigger, $Settings, [switch]$Force)
    if ($Xml) { $script:Fixture.Tasks = @($Xml | ConvertFrom-Json) }
    else { $script:Fixture.Tasks = @([PSCustomObject]@{ TaskName=$TaskName; TaskPath=$TaskPath; Actions=@($Action) }) }
    $script:Fixture.Calls += "register:$TaskPath"
}
function New-ScheduledTaskAction { param($Execute, $Argument); [PSCustomObject]@{ Execute=$Execute; Arguments=$Argument } }
function New-ScheduledTaskTrigger { param([switch]$AtLogOn, $User); @{} }
function New-ScheduledTaskSettingsSet { param($RestartCount, $RestartInterval, $ExecutionTimeLimit); @{} }
function Get-CimInstance { param($ClassName, $Filter); @() }
function Stop-Process { throw 'Unexpected process operation in isolated test' }
function Wait-Process { throw 'Unexpected process operation in isolated test' }
function Get-Process { throw 'Unexpected process operation in isolated test' }
function Start-Process { param($FilePath); $script:Fixture.Calls += 'browser' }
function Start-Sleep { param($Seconds) }
function Invoke-RestMethod {
    param($Uri, $TimeoutSec)
    if (-not $script:Fixture.Ready) { throw 'Fixture not ready' }
    $script:Fixture.Hash
}
function Get-HttpsFile {
    param([string]$Url, [string]$Destination)
    if ($Url -eq 'https://fixture.invalid/manifest.json') {
        @{ platforms = @{ 'windows-amd64' = @{ file='fixture.zip'; sha256=$script:Fixture.ManifestHash } } } | ConvertTo-Json -Depth 8 | Set-Content $Destination
    } elseif ($Url -eq 'https://fixture.invalid/fixture.zip') { Copy-Item $script:Fixture.Archive $Destination }
    else { throw 'Unexpected network request in fixture' }
}
function Make-Archive([string]$Tag, [switch]$Incomplete) {
    $Directory = Join-Path $Temporary ('package-' + $Tag)
    New-Item -ItemType Directory -Force -Path (Join-Path $Directory 'runtime'), (Join-Path $Directory 'web_dist') | Out-Null
    'fixture executable; never run' | Set-Content (Join-Path $Directory 'silencer-gateway.exe')
    if (-not $Incomplete) { 'fixture runtime; never run' | Set-Content (Join-Path $Directory 'runtime/mitmdump.exe') }
    New-Item -ItemType Directory -Force -Path (Join-Path $Directory 'lib') | Out-Null
    'fixture dependency; never run' | Set-Content (Join-Path $Directory 'lib/engine.dll')
    $Tag | Set-Content (Join-Path $Directory 'web_dist/index.html')
    $script:Fixture.Archive = Join-Path $Temporary ($Tag + '.zip')
    Compress-Archive -Path (Join-Path $Directory '*') -DestinationPath $script:Fixture.Archive
    $script:Fixture.Hash = (Get-FileHash $script:Fixture.Archive -Algorithm SHA256).Hash.ToLowerInvariant()
    $script:Fixture.ManifestHash = $script:Fixture.Hash
}
function Invoke-FixtureInstaller([switch]$Remove) {
    $env:SILENCER_DATA_DIR = $script:Fixture.Data
    $env:SILENCER_RELEASE_BASE = 'https://fixture.invalid'
    $Uninstall = $Remove.IsPresent
    . ([ScriptBlock]::Create($Initialize))
    . ([ScriptBlock]::Create($Main))
}
function Ensure([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
function Expect-Failure([ScriptBlock]$Action) {
    $Failed = $false
    try { & $Action | Out-Null } catch { $Failed = $true }
    Ensure $Failed 'Expected operation to fail'
}
$Passed = 0
try {
    Make-Archive 'one'
    Invoke-FixtureInstaller | Out-Null
    $First = $script:Fixture.Hash
    $Current = Join-Path $script:Fixture.Data 'current'
    Ensure ((Get-Content $Current -Raw).Trim() -eq $First) 'First install did not commit'
    $Sentinel = Join-Path $script:Fixture.Data 'user-data.bin'
    [IO.File]::WriteAllBytes($Sentinel, [byte[]]@(0, 255, 18, 42))
    $SentinelHash = (Get-FileHash $Sentinel).Hash
    Invoke-FixtureInstaller | Out-Null
    Ensure (@(Get-ChildItem (Join-Path $script:Fixture.Data 'versions') -Directory).Count -eq 1) 'Repeated install created a duplicate'
    $Passed += 2

    $Version = Join-Path (Join-Path $script:Fixture.Data 'versions') $First
    'damaged executable' | Set-Content (Join-Path $Version 'silencer-gateway.exe')
    Remove-Item (Join-Path $Version 'lib/engine.dll')
    Invoke-FixtureInstaller | Out-Null
    Ensure ((Get-Content (Join-Path $Version 'silencer-gateway.exe') -Raw).Trim() -eq 'fixture executable; never run') 'Repair reused a damaged executable'
    Ensure (Test-Path (Join-Path $Version 'lib/engine.dll')) 'Repair did not restore a missing dependency'
    $Passed++

    $OldGatewayHash = (Get-FileHash (Join-Path $Version 'silencer-gateway.exe')).Hash
    $script:Fixture.Ready = $false
    Expect-Failure { Invoke-FixtureInstaller }
    Ensure ((Get-FileHash (Join-Path $Version 'silencer-gateway.exe')).Hash -eq $OldGatewayHash) 'Same-version rollback lost previous files'
    Ensure ((Get-Content $Current -Raw).Trim() -eq $First) 'Same-version rollback lost installation identity'
    $script:Fixture.Ready = $true
    $Passed++

    Make-Archive 'two'
    Invoke-FixtureInstaller | Out-Null
    $Second = $script:Fixture.Hash
    Ensure ($Second -ne $First -and (Get-Content $Current -Raw).Trim() -eq $Second) 'Update did not commit'
    Ensure ((Get-FileHash $Sentinel).Hash -eq $SentinelHash) 'Update changed user data'
    $Passed++

    $script:Fixture.ManifestHash = '0' * 64
    Expect-Failure { Invoke-FixtureInstaller }
    Ensure ((Get-Content $Current -Raw).Trim() -eq $Second) 'Checksum failure changed the active version'
    $script:Fixture.ManifestHash = $script:Fixture.Hash
    $Passed++

    Make-Archive 'incomplete' -Incomplete
    Expect-Failure { Invoke-FixtureInstaller }
    Ensure ((Get-Content $Current -Raw).Trim() -eq $Second) 'Incomplete bundle changed the active version'
    $Passed++

    Make-Archive 'not-ready'
    $script:Fixture.Ready = $false
    Expect-Failure { Invoke-FixtureInstaller }
    Ensure ((Get-Content $Current -Raw).Trim() -eq $Second) 'Readiness failure did not restore the previous version'
    Ensure ($script:Fixture.Tasks.Count -eq 1) 'Rollback lost the previous task'
    $script:Fixture.Ready = $true
    $Passed++

    $Own = $script:Fixture.Tasks
    $ForeignAction = [PSCustomObject]@{ Execute='powershell.exe'; Arguments='-File other-installation.ps1' }
    $script:Fixture.Tasks = @([PSCustomObject]@{ TaskName='Silencer Local'; TaskPath='\'; Actions=@($ForeignAction) })
    $script:Fixture.Calls = @()
    Expect-Failure { Invoke-FixtureInstaller }
    Expect-Failure { Invoke-FixtureInstaller -Remove }
    Ensure ($script:Fixture.Calls.Count -eq 0) 'Foreign task was modified'
    Ensure ((Get-Content $Current -Raw).Trim() -eq $Second) 'Foreign-task rejection changed installation data'
    $script:Fixture.Tasks = $Own
    $Passed += 2

    $script:Fixture.FailEnumeration = $true
    Expect-Failure { Invoke-FixtureInstaller -Remove }
    Ensure ($script:Fixture.Calls.Count -eq 0) 'Unknown task state was treated as absent'
    $script:Fixture.FailEnumeration = $false
    $Passed++

    $script:Fixture.DenyUnregister = $true
    Expect-Failure { Invoke-FixtureInstaller -Remove }
    Ensure (Test-Path (Join-Path $script:Fixture.Data '.silencer-local-install')) 'Failed uninstall lost its retry marker'
    $script:Fixture.DenyUnregister = $false
    $Passed++

    Invoke-FixtureInstaller -Remove | Out-Null
    Ensure ($script:Fixture.Tasks.Count -eq 0 -and -not (Test-Path $Current)) 'Uninstall did not remove its task and launcher'
    Ensure ((Get-FileHash $Sentinel).Hash -eq $SentinelHash) 'Uninstall changed user data'
    $Passed++
    Make-Archive 'reinstall'
    Invoke-FixtureInstaller | Out-Null
    Ensure ((Get-FileHash $Sentinel).Hash -eq $SentinelHash) 'Reinstall changed user data'
    $Passed++
    Write-Output "Windows installer isolated behavior tests passed: $Passed"
} finally {
    foreach ($Item in Get-ChildItem Env: | Where-Object { $_.Name.StartsWith('SILENCER_') }) { [Environment]::SetEnvironmentVariable($Item.Name, $null, 'Process') }
    foreach ($Name in $SavedEnvironment.Keys) { [Environment]::SetEnvironmentVariable($Name, $SavedEnvironment[$Name], 'Process') }
    Remove-Item -Recurse -Force $Temporary
}
