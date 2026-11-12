# Runs once as the new, non-elevated Cage workbench user. The administrator
# already provisioned the verified MSIX machine-wide; finishing registration
# here makes Windows consider it usable in this particular user session.
[CmdletBinding()]
param([Parameter(Mandatory)][string]$BundleDirectory,[Parameter(Mandatory)][string]$BridgeDirectory)
$ErrorActionPreference='Stop'
$user=[Security.Principal.WindowsIdentity]::GetCurrent()
if ([Security.Principal.WindowsPrincipal]::new($user).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run as the dedicated standard guest user.' }
. (Join-Path $PSScriptRoot 'claude-desktop-windows.ps1')
$packages=Get-Content -LiteralPath (Join-Path $BundleDirectory 'resolved-workbench-packages.json') -Raw | ConvertFrom-Json
$wanted=[version]($packages.desktop.version+'.0')
$registered=Get-RegisteredClaude
if (!$registered -or [version]$registered.Version -lt $wanted) {
    Add-AppxPackage -RegisterByFamilyName -MainPackage 'Claude_pzs8sxrjxfjjc' -ErrorAction Stop
    $registered=Get-RegisteredClaude
    if (!$registered -or [version]$registered.Version -lt $wanted) { throw 'The signed-in user did not register the selected Claude Desktop version.' }
}
& (Join-Path $PSScriptRoot 'install-cli.ps1') -BundleDirectory $BundleDirectory -BridgeDirectory $BridgeDirectory -WorkbenchSetup
