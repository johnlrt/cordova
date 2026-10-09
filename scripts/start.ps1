param([string]$Listen = '127.0.0.1:8080')
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
Push-Location $projectRoot
try {
    $binary = Join-Path $projectRoot 'bin/cordova-gateway.exe'
    if (-not (Test-Path -LiteralPath $binary)) {
        if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
            throw 'Install Go 1.27.2, or extract the Windows release bundle with bin/cordova-gateway.exe.'
        }
        New-Item -ItemType Directory -Path 'bin' -Force | Out-Null
        Push-Location 'server'
        try {
            & go build -trimpath -o $binary ./cmd/gateway
            if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
        } finally { Pop-Location }
    }
    if (-not $env:CORDOVA_API_KEY -and -not (Test-Path -LiteralPath '.local/api-key.txt')) {
        & $binary -init-key
        if ($LASTEXITCODE -ne 0) { throw 'Key initialization failed.' }
    }
    & $binary -listen $Listen
    if ($LASTEXITCODE -ne 0) { throw 'Gateway exited with an error.' }
} finally { Pop-Location }
