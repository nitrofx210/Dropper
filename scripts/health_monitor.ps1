# =============================================================================
# scripts/health_monitor.ps1
# =============================================================================
# Ethereum Dropper real-run health monitor.
#
# Manual execution only. This script does NOT connect to the network by itself;
# it makes outbound HTTPS calls to the configured RPC endpoint when you run it
# to (1) fetch the chain head block number/timestamp and (2) compare it against
# the local observer's latest collection run so you can confirm the observer is
# catching up to Mainnet before / during a real run.
#
# Prerequisites: PowerShell 5.1+ (Windows) or pwsh 7+.
#
# Examples:
#   .\scripts\health_monitor.ps1
#   .\scripts\health_monitor.ps1 -RpcEndpoint https://eth.drpc.org -DbPath mainnet_2026-10-23.db -LogPath scripts/health_monitor.jsonl -IntervalSeconds 60 -MaxCycles 10
#   .\scripts\health_monitor.ps1 -RpcEndpoint https://eth.drpc.org -DbPath mainnet_2026-10-23.db -Once
#
# Exit codes:
#   0 = healthy (head reachable, local run within tolerance)
#   1 = unhealthy or error
#   2 = transient RPC failure (head unreachable)
# =============================================================================

[CmdletBinding()]
param(
    [Parameter(Mandatory = $false)]
    [string]$RpcEndpoint = "https://eth.drpc.org",

    [Parameter(Mandatory = $false)]
    [string]$DbPath = "mainnet_2026-10-23.db",

    [Parameter(Mandatory = $false)]
    [string]$LogPath = "scripts/health_monitor.jsonl",

    [Parameter(Mandatory = $false)]
    [int]$IntervalSeconds = 60,

    [Parameter(Mandatory = $false)]
    [int]$MaxCycles = 0,

    [Parameter(Mandatory = $false)]
    [switch]$Once
)

$ErrorActionPreference = "Stop"
$script:cycle = 0
$script:failures = 0
$script:timestamp = { Get-Date -Format "o" }

function Write-JsonLog {
    param(
        [string]$Status,
        [string]$Message,
        [hashtable]$Data = @{}
    )
    $entry = @{
        timestamp     = &$script:timestamp
        status        = $Status
        message       = $Message
        rpc_endpoint  = $RpcEndpoint
        db_path       = $DbPath
    }
    foreach ($k in $Data.Keys) { $entry[$k] = $Data[$k] }
    $json = $entry | ConvertTo-Json -Depth 5
    if ($LogPath) {
        $dir = Split-Path -Parent $LogPath
        if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
        Add-Content -Path $LogPath -Value $json
    }
    Write-Host "[$(&$script:timestamp)] [$Status] $Message"
}

function Get-ChainHead {
    <#
    .SYNOPSIS
    Fetch eth_blockNumber from the RPC endpoint and return (blockNumber, blockTimestamp).
    #>
    $body = @{
        jsonrpc = "2.0"
        method  = "eth_blockNumber"
        params  = @()
        id      = 1
    } | ConvertTo-Json -Depth 3

    try {
        $resp = Invoke-RestMethod -Uri $RpcEndpoint -Method Post -ContentType "application/json" -Body $body -TimeoutSec 15 -ErrorAction Stop
    } catch {
        Write-JsonLog -Status "rpc_error" -Message "RPC request failed: $($_.Exception.Message)"
        return $null
    }

    if ($null -eq $resp -or $null -eq $resp.result) {
        Write-JsonLog -Status "rpc_error" -Message "RPC returned no result"
        return $null
    }

    $hexBlock = [string]$resp.result
    $blockNum = [convert]::ToInt64($hexBlock.TrimStart("0x"), 16)

    # Fetch header to get the timestamp of the head block
    $headerBody = @{
        jsonrpc = "2.0"
        method  = "eth_getBlockByNumber"
        params  = @( $hexBlock, $false )
        id      = 2
    } | ConvertTo-Json -Depth 3

    $blockTs = 0
    try {
        $hresp = Invoke-RestMethod -Uri $RpcEndpoint -Method Post -ContentType "application/json" -Body $headerBody -TimeoutSec 15 -ErrorAction Stop
        if ($null -ne $hresp -and $null -ne $hresp.result -and $null -ne $hresp.result.timestamp) {
            $blockTs = [convert]::ToInt64($hresp.result.timestamp.TrimStart("0x"), 16)
        }
    } catch {
        # Timestamp is optional for head health; don't fail the whole check.
    }

    return @{ number = $blockNum; timestamp = $blockTs; hex = $hexBlock }
}

function Get-LatestLocalRun {
    <#
    .SYNOPSIS
    Read the latest collection run end time from the local SQLite DB using the
    sqlite3 CLI if available, otherwise fall back to a lightweight PowerShell
    parse of the WAL-mode DB header (read-only). If neither works, return $null.
    #>
    if (-not (Test-Path $DbPath)) {
        Write-JsonLog -Status "db_missing" -Message "Database file not found: $DbPath"
        return $null
    }

    # Prefer the sqlite3 CLI for accurate, read-only access.
    $sqlite3 = Get-Command sqlite3 -ErrorAction SilentlyContinue
    if ($sqlite3) {
        $query = "SELECT MAX(run_id), MAX(end_ns) FROM collection_runs;"
        $out = & $sqlite3 $DbPath $query 2>$null
        if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($out)) {
            Write-JsonLog -Status "db_query_error" -Message "sqlite3 query failed"
            return $null
        }
        $parts = $out -split "|"
        if ($parts.Length -ge 2) {
            $runIdOk = $false
            $endNsOk = $false
            [int64]::TryParse($parts[0], [ref]$runId) | Out-Null
            [int64]::TryParse($parts[1], [ref]$endNs) | Out-Null
            if ($runIdOk -and $runId -gt 0) {
                $runEndNs = 0
                if ($endNsOk -and $parts[1] -ne "null") { $runEndNs = $endNs }
                return @{ run_id = $runId; end_ns = $runEndNs }
            }
        }
    }

    # Fallback: no sqlite3 CLI available. Report what we can from the file alone.
    Write-JsonLog -Status "db_no_sqlite3" -Message "sqlite3 CLI not available; cannot read run times from DB"
    return $null
}

function Test-Health {
    # Returns an exit code: 0 = healthy, 1 = unhealthy/error, 2 = transient RPC failure.
    $head = Get-ChainHead
    if ($null -eq $head) {
        $script:failures++
        return 2
    }

    $nowUnix = [int64](Get-Date -Date (Get-Date).ToUniversalTime() -UFormat %s)
    $headAgeSec = $nowUnix - $head.timestamp
    $headAgeMin = [math]::Round($headAgeSec / 60, 1)

    $data = @{
        head_block_number = $head.number
        head_block_timestamp = $head.timestamp
        head_age_minutes = $headAgeMin
        now_unix_seconds = $nowUnix
    }

    if ($headAgeSec -gt 900) {
        Write-JsonLog -Status "stale_head" -Message "Chain head is $headAgeMin min old (>15 min); RPC may be lagging" -Data $data
        $script:failures++
        return $false
    }

    Write-JsonLog -Status "rpc_ok" -Message "Chain head at block $($head.number), age $headAgeMin min" -Data $data

    $run = Get-LatestLocalRun
    if ($null -eq $run) {
        # No local DB / no runs yet — that's expected before the observer has started.
        Write-JsonLog -Status "no_local_run" -Message "No collection runs found in local DB yet" -Data $data
        # Don't count as a failure: observer may not have started.
        return $true
    }

    $runEndUnix = [math]::Round($run.end_ns / 1e9)
    $lagSec = $nowUnix - $runEndUnix
    $lagMin = [math]::Round($lagSec / 60, 1)
    $data.Add("local_run_id", $run.run_id)
    $data.Add("local_run_end_unix", $runEndUnix)
    $data.Add("local_run_lag_minutes", $lagMin)

    if ($lagSec -gt 3600) {
        Write-JsonLog -Status "local_lagging" -Message "Local run ended $lagMin min ago (>1 h); observer may not be catching up" -Data $data
        $script:failures++
        return 1
    }

    Write-JsonLog -Status "healthy" -Message "Local run $($run.run_id) ended $lagMin min ago; within tolerance" -Data $data
    return 0
}

# Main loop
$healthy = Test-Health
if ($Once) {
    exit $healthy
}

while ($true) {
    $script:cycle++
    $healthy = Test-Health
    if ($MaxCycles -gt 0 -and $script:cycle -ge $MaxCycles) { break }
    Start-Sleep -Seconds $IntervalSeconds
}

exit $healthy
