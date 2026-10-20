# Native Windows-only ACL verification in an isolated temporary directory.
$ErrorActionPreference = 'Stop'
if ([Environment]::OSVersion.Platform -ne 'Win32NT') { throw 'Run this test on Windows' }
$Source = Join-Path $PSScriptRoot '../gateway/internal/gateway/capture_assets/install.ps1'
$Tokens = $null; $Errors = $null
$Ast = [System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path $Source), [ref]$Tokens, [ref]$Errors)
if ($Errors) { throw 'Installer syntax errors' }
foreach ($Definition in $Ast.EndBlock.Statements | Where-Object { $_ -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $_.Name -in @('Get-InstallerIdentity','Protect-InstallationDirectory','Protect-InstallationItem') }) {
    . ([ScriptBlock]::Create($Definition.Extent.Text))
}
$Temporary = Join-Path ([IO.Path]::GetTempPath()) ('silencer-installer-acl-' + [Guid]::NewGuid().ToString('N'))
$CaptureRoot = Join-Path $Temporary 'data'
$Outside = Join-Path $Temporary 'outside'
try {
    New-Item -ItemType Directory -Path $CaptureRoot, (Join-Path $CaptureRoot 'nested'), $Outside | Out-Null
    $Files = @((Join-Path $CaptureRoot 'control.json'), (Join-Path $CaptureRoot 'silencer.db'), (Join-Path $CaptureRoot 'nested/private.json'))
    foreach ($File in $Files) {
        'private fixture' | Set-Content -LiteralPath $File
        $Public = [Security.AccessControl.FileSecurity]::new()
        $Public.SetAccessRuleProtection($true, $false)
        $Public.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new('S-1-1-0'), 'FullControl', 'Allow'))
        Set-Acl -LiteralPath $File -AclObject $Public
    }
    Protect-InstallationDirectory
    $Allowed = @((Get-InstallerIdentity).User.Value, 'S-1-5-18')
    foreach ($File in $Files) {
        $Acl = Get-Acl -LiteralPath $File
        if (-not $Acl.AreAccessRulesProtected) { throw 'Sensitive file still inherits public permissions' }
        foreach ($Rule in $Acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
            if ($Rule.IdentityReference.Value -notin $Allowed) { throw 'Sensitive file still grants public access' }
        }
    }
    $Before = (Get-Acl -LiteralPath $Outside).Sddl
    New-Item -ItemType Junction -Path (Join-Path $CaptureRoot 'external') -Target $Outside | Out-Null
    $Rejected = $false
    try { Protect-InstallationDirectory } catch { $Rejected = $true }
    if (-not $Rejected -or (Get-Acl -LiteralPath $Outside).Sddl -ne $Before) { throw 'Reparse target was traversed' }
    Write-Output 'Windows installer native ACL tests passed'
} finally {
    $Junction = Join-Path $CaptureRoot 'external'
    if (Test-Path -LiteralPath $Junction) { [IO.Directory]::Delete($Junction) }
    if (Test-Path -LiteralPath $Temporary) { Remove-Item -LiteralPath $Temporary -Recurse -Force }
}
