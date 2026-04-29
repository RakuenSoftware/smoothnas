param(
    [Parameter(Mandatory=$true)][string]$Server,
    [Parameter(Mandatory=$true)][string]$Share,
    [string]$Drive = "S:",
    [int]$Files = 1000
)

$ErrorActionPreference = "Stop"
$unc = "\\$Server\$Share"
if (Get-PSDrive -Name $Drive.TrimEnd(":") -ErrorAction SilentlyContinue) {
    net use $Drive /delete /y | Out-Null
}
net use $Drive $unc /persistent:no | Out-Null
try {
    $root = Join-Path $Drive "smoothnas-windows-soak"
    New-Item -ItemType Directory -Force -Path $root | Out-Null
    1..$Files | ForEach-Object {
        $path = Join-Path $root ("file-{0:D5}.dat" -f $_)
        [IO.File]::WriteAllBytes($path, [Text.Encoding]::UTF8.GetBytes("smoothnas $_"))
        $got = [IO.File]::ReadAllText($path)
        if ($got -ne "smoothnas $_") {
            throw "round-trip mismatch for $path"
        }
    }
    Get-ChildItem $root -File | Remove-Item -Force
}
finally {
    net use $Drive /delete /y | Out-Null
}
