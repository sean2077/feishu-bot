# Temporary fixture trust, restricted to disposable GitHub-hosted runners.
# Import-Certificate and the Certificate provider retain normal Windows trust
# and authorization. No policy or CryptoAPI protection flags are changed.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('import', 'inspect', 'remove')]
    [string] $Operation,
    [Parameter(Mandatory = $true)]
    [ValidateSet('CurrentUser', 'LocalMachine')]
    [string] $StoreScope,
    [Parameter(Mandatory = $true)]
    [string] $Certificate,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[a-fA-F0-9]{40}$')]
    [string] $Thumbprint,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[a-fA-F0-9]{64}$')]
    [string] $Sha256
)

$ErrorActionPreference = 'Stop'
if ($env:GITHUB_ACTIONS -ne 'true' -or
    $env:RUNNER_ENVIRONMENT -ne 'github-hosted' -or
    $env:FEISHU_ARTIFACT_SYSTEM_TRUST -ne '1') {
    throw 'OS trust fixtures require explicit opt-in on an isolated GitHub-hosted runner'
}
if ($StoreScope -eq 'LocalMachine') {
    $principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Temporary LocalMachine Root fixture requires an existing administrator token; no elevation attempted'
    }
}

function Assert-CertificateBytes($Value) {
    if ($Value.Thumbprint -ne $Thumbprint) {
        throw 'Temporary certificate thumbprint does not match the cleanup journal'
    }
    $hasher = [System.Security.Cryptography.SHA256]::Create()
    try {
        $digest = [BitConverter]::ToString($hasher.ComputeHash($Value.RawData)).Replace('-', '')
        if ($digest -ne $Sha256) {
            throw 'Temporary certificate SHA256 does not match the cleanup journal'
        }
    } finally {
        $hasher.Dispose()
    }
}

function Read-FixtureCertificate {
    $store = [System.Security.Cryptography.X509Certificates.X509Store]::new('Root', $StoreScope)
    try {
        $flags = [System.Security.Cryptography.X509Certificates.OpenFlags]::ReadOnly -bor
                 [System.Security.Cryptography.X509Certificates.OpenFlags]::OpenExistingOnly
        $store.Open($flags)
        $matches = $store.Certificates.Find('FindByThumbprint', $Thumbprint, $false)
        if ($matches.Count -eq 0) { return $null }
        if ($matches.Count -ne 1) { throw 'Temporary certificate thumbprint is ambiguous' }
        Assert-CertificateBytes $matches[0]
        return $matches[0]
    } finally {
        $store.Dispose()
    }
}

try {
    $existing = Read-FixtureCertificate
    if ($Operation -eq 'import') {
        if ($null -ne $existing) { throw "Fixture certificate already exists in $StoreScope Root" }
        $inputCertificate = [System.Security.Cryptography.X509Certificates.X509Certificate2]::new(
            [System.IO.File]::ReadAllBytes($Certificate))
        try { Assert-CertificateBytes $inputCertificate } finally { $inputCertificate.Dispose() }
        Import-Certificate -FilePath $Certificate -CertStoreLocation "Cert:\$StoreScope\Root" | Out-Null
    } elseif ($Operation -eq 'remove' -and $null -ne $existing) {
        Remove-Item -LiteralPath "Cert:\$StoreScope\Root\$Thumbprint"
    }
    $present = $null -ne (Read-FixtureCertificate)
    if ($Operation -eq 'import' -and -not $present) { throw 'Temporary certificate import was not confirmed' }
    if ($Operation -eq 'remove' -and $present) { throw 'Temporary certificate removal was not confirmed' }
    @{ present = $present } | ConvertTo-Json -Compress
} catch {
    [Console]::Error.WriteLine("{0} Root {1}: {2}", $StoreScope, $Operation, $_.Exception.Message)
    exit 1
}
