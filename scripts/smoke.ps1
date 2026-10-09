param([string]$BaseUrl = 'http://127.0.0.1:8080')
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$key = $env:CORDOVA_API_KEY
if (-not $key) { $key = (Get-Content -LiteralPath (Join-Path $projectRoot '.local/api-key.txt') -Raw).Trim() }
$headers = @{ Authorization = "Bearer $key" }
$health = Invoke-RestMethod "$BaseUrl/healthz"
if ($health.mode -ne 'mock') { throw 'Unexpected gateway mode.' }
$models = Invoke-RestMethod "$BaseUrl/v1/models" -Headers $headers
if ($models.data[0].id -ne 'cordova-chat-small-v1') { throw 'Missing catalog model.' }
$body = @{model='cordova-chat-small-v1'; messages=@(@{role='user';content='Say hello'});stream=$true;stream_options=@{include_usage=$true}} | ConvertTo-Json -Depth 6
$response = Invoke-WebRequest "$BaseUrl/v1/chat/completions" -Method Post -Headers $headers -ContentType 'application/json' -Body $body -UseBasicParsing
$content = $response.Content
if ($content -is [byte[]]) { $content = [Text.Encoding]::UTF8.GetString($content) }
if (-not $content.Contains('[MOCK]') -or -not $content.Contains('data: [DONE]')) { throw 'Incomplete SSE response.' }
Write-Output 'PASS: health, authenticated model catalog and complete mock SSE response.'
Write-Output 'This is synthetic transport verification. No model inference or billing was performed.'
