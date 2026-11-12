package guestos

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the real PowerShell function and native argument passing. The small
// executable stands in for the already tested Go HTTPS downloader, not the API.
func TestWindowsPackageDownloadUsesBundledHostDownloader(t *testing.T) {
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		shell, err = exec.LookPath("powershell.exe")
	}
	if err != nil {
		t.Skip("PowerShell unavailable")
	}
	root := t.TempDir()
	kit := filepath.Join(root, "guest-os")
	if err := os.Mkdir(kit, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../../scripts/guest-os/windows-workbench-packages.ps1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kit, "windows-workbench-packages.ps1"), source, 0600); err != nil {
		t.Fatal(err)
	}
	fixture := `package main
import("encoding/json";"os";"path/filepath";"fmt")
func main(){
 a:=os.Args
 if len(a)>2 && a[2]=="https://example.invalid/metadata" {
  if len(a)!=6 || a[1]!="host-download" || a[4]!="metadata" || a[5]!="*/*" {fmt.Printf("bad metadata arguments: %q\n",a);os.Exit(92)}
  os.WriteFile(a[3],[]byte("release metadata"),0600);return
 }
 if len(a)!=8 || a[1]!="host-download" || a[2]!="https://example.invalid/package" || a[4]!="package.zip" || a[5]!="*/*" || filepath.Base(a[6])!=".downloads" || len(a[7])!=64 {fmt.Printf("bad native arguments: %q\n",a);os.Exit(91)}
 file,e:=os.OpenFile(os.Getenv("PACKAGE_FIXTURE_LOG"),os.O_CREATE|os.O_WRONLY|os.O_APPEND,0600);if e!=nil{panic(e)};json.NewEncoder(file).Encode(a[1:]);file.Close()
 os.WriteFile(a[3],[]byte("verified package"),0600)
 fmt.Println("CAGE_HOST_BYTES package.zip 16 16")
 if os.Getenv("PACKAGE_FIXTURE_FAIL")=="1" {os.Exit(23)}
}`
	src := filepath.Join(root, "fixture.go")
	if err := os.WriteFile(src, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(root, "cage.exe"), src)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v %s", err, out)
	}
	script := `param([string]$Root)
$ErrorActionPreference='Stop'
. (Join-Path $Root 'guest-os/windows-workbench-packages.ps1')
$cache=Join-Path $Root 'cache'
$null=New-Item -ItemType Directory -Path $cache
$original=Join-Path $Root 'original'
[IO.File]::WriteAllText($original,'verified package')
$spec=[pscustomobject]@{url='https://example.invalid/package';filename='package.zip';sha256=(Get-FileHash -LiteralPath $original).Hash}
$env:PACKAGE_FIXTURE_LOG=Join-Path $Root 'requests.jsonl'
$result=@(Get-PinnedPackage $spec $cache)
if($result.Count -ne 1 -or $result[0] -cne (Join-Path $cache 'package.zip')){throw 'Progress output contaminated the returned file path.'}
$result=@(Get-PinnedPackage $spec $cache)
if(@(Get-Content -LiteralPath $env:PACKAGE_FIXTURE_LOG).Count -ne 1){throw 'Verified cached file was downloaded again.'}
$spec.sha256='0'*64
$env:PACKAGE_FIXTURE_FAIL='1'
$failed=$false
try{$null=Get-PinnedPackage $spec $cache}catch{$failed=$_.Exception.Message -like 'Package download failed:*'}
if(!$failed){throw 'Native downloader failure was ignored.'}
if([IO.File]::ReadAllText((Join-Path $cache 'package.zip')) -cne 'verified package'){throw 'Failed replacement damaged the old cache.'}
if(@(Get-ChildItem -LiteralPath $cache -Filter '*.download').Count){throw 'Unverified staging file survived.'}
Remove-Item Env:PACKAGE_FIXTURE_FAIL
$failed=$false
try{$null=Get-PinnedPackage $spec $cache}catch{$failed=$_.Exception.Message -like 'Package verification failed:*'}
if(!$failed){throw 'Package checksum mismatch was accepted.'}
$metadata=Join-Path $Root 'metadata.txt'
Invoke-WorkbenchDownload 'https://example.invalid/metadata' $metadata 'metadata' $cache
if([IO.File]::ReadAllText($metadata) -cne 'release metadata'){throw 'Metadata download changed the native argument contract.'}
Write-Output 'Passed bundled downloader arguments, cache reuse, progress isolation, failure preservation and checksum rejection.'
`
	path := filepath.Join(root, "test.ps1")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), shell, "-NoLogo", "-NoProfile", "-File", path, "-Root", root)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}
