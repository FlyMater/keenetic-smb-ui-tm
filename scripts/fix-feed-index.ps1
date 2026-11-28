param(
  [Parameter(Mandatory = $true)]
  [ValidatePattern('^v[0-9]+\.[0-9]+\.[0-9]+$')]
  [string]$Tag
)

$ErrorActionPreference = 'Stop'
$env:GCM_INTERACTIVE = 'never'; $env:GIT_TERMINAL_PROMPT = '0'
$cred = "protocol=https`nhost=github.com`n`n" | git credential fill 2>$null
$token = ($cred -split "`n" | Where-Object { $_ -like 'password=*' }) -replace 'password=', ''
if (-not $token) { throw 'no token' }
$h = @{ Authorization = "token $token"; 'User-Agent' = 'keenetic-smb-ui-tm'; Accept = 'application/vnd.github+json' }
$repo = 'FlyMater/keenetic-smb-ui-tm'
$rel = Join-Path (Split-Path -Parent $PSScriptRoot) 'dist/feed/release'

foreach ($name in 'Packages', 'Packages.gz') {
  if (-not (Test-Path -LiteralPath (Join-Path $rel $name) -PathType Leaf)) {
    throw "Missing local feed index: $name"
  }
}

$release = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/tags/$Tag" -Headers $h -TimeoutSec 30
Write-Host "release id=$($release.id) assets=$($release.assets.Count)"

# remove old index assets, keep everything else
foreach ($a in $release.assets | Where-Object { $_.name -eq 'Packages' -or $_.name -eq 'Packages.gz' }) {
  Invoke-RestMethod "https://api.github.com/repos/$repo/releases/assets/$($a.id)" -Method Delete -Headers $h -TimeoutSec 30 | Out-Null
  Write-Host "deleted old $($a.name)"
}

foreach ($name in 'Packages', 'Packages.gz') {
  $f = Join-Path $rel $name
  $url = "https://uploads.github.com/repos/$repo/releases/$($release.id)/assets?name=$name"
  Invoke-RestMethod $url -Method Post -Headers $h -ContentType 'application/octet-stream' -InFile $f -TimeoutSec 120 | Out-Null
  Write-Host "uploaded $name ($((Get-Item $f).Length) bytes)"
}

$final = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/tags/$Tag" -Headers $h -TimeoutSec 30
Write-Host "assets now: $($final.assets.Count)"
