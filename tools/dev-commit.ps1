param(
  [ValidateSet("feat","fix","chore","docs","refactor","perf","test","ops","schema")]
  [string]$Type = "feat",
  [string]$Scope = "",
  [Parameter(Mandatory=$true)][string]$Message,
  [switch]$NoChangelog
)

function HasChanges {
  $status = git status --porcelain
  return -not [string]::IsNullOrWhiteSpace($status)
}

if (-not (HasChanges)) {
  Write-Host "No changes detected. Nothing to commit."
  exit 0
}

# Conventional commit header
if ([string]::IsNullOrWhiteSpace($Scope)) {
  $header = "${Type}: $Message"
} else {
  $header = "${Type}($Scope): $Message"
}

# Optionally update CHANGELOG.md under [Unreleased]
if (-not $NoChangelog) {
  $path = "CHANGELOG.md"
  if (Test-Path $path) {
    $ch = Get-Content $path -Raw
    # Which section?
    $section =
      ( $Type -in @("feat") )    ? "Added" :
      ( $Type -in @("fix") )     ? "Fixed" :
      "Changed"

    # Ensure Unreleased section exists
    if ($ch -notmatch "## \[Unreleased\]") {
      $ch = "# Changelog`r`n`r`n## [Unreleased]`r`n### Added`r`n- None`r`n`r`n### Changed`r`n- None`r`n`r`n### Fixed`r`n- None`r`n`r`n" + $ch
    }

    # Replace "- None" with first item, otherwise append a new bullet
    $pattern = "(?s)(## \[Unreleased\].*?### $section\s*)(- None|)(.*?)(?=### |\Z)"
    $repl = {
      param($m)
      $prefix = $m.Groups[1].Value
      $rest   = $m.Groups[3].Value
      if ($m.Groups[2].Value -ne "") {
        return "$prefix- $Message$rest"
      } else {
        # add bullet at the end of this section
        return "$prefix$rest`r`n- $Message"
      }
    }
    $updated = [regex]::Replace($ch, $pattern, $repl, 1)
    Set-Content -Path $path -Value $updated -NoNewline
    git add CHANGELOG.md | Out-Null
  } else {
    Write-Warning "CHANGELOG.md not found; skipping changelog update."
  }
}

git add .
git commit -m $header
git push
Write-Host "Committed and pushed: $header"
