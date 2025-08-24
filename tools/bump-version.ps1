param(
  [Parameter(Mandatory=$true)][string]$Version,
  [switch]$Tag
)

function Fail($msg) { Write-Error $msg; exit 1 }

if ($Version -notmatch "^\d+\.\d+\.\d+$") { Fail "Version must be SemVer like 0.1.0" }
$today = (Get-Date -Format "yyyy-MM-dd")

$changelogPath = "CHANGELOG.md"
if (!(Test-Path $changelogPath)) { Fail "CHANGELOG.md not found" }

$ch = Get-Content $changelogPath -Raw

# Find Unreleased section
$pattern = "(?s)## \[Unreleased\]\s*(.*?)(?=## \[|\Z)"
$m = [regex]::Match($ch, $pattern)
if (!$m.Success) { Fail "Couldn't find [Unreleased] section in CHANGELOG.md" }

$unreleasedContent = $m.Groups[1].Value.Trim()
if ([string]::IsNullOrWhiteSpace($unreleasedContent)) {
  Write-Host "No changes under [Unreleased]; proceeding to create empty release notes."
}

# Build new section for version
$newSection = "## [$Version] - $today`r`n"
if ($unreleasedContent) {
  $newSection += "$unreleasedContent`r`n`r`n"
} else {
  $newSection += "_No notable changes._`r`n`r`n"
}

# Fresh Unreleased stub
$freshUnreleased = "## [Unreleased]`r`n### Added`r`n- None`r`n`r`n### Changed`r`n- None`r`n`r`n### Fixed`r`n- None`r`n"

# Replace
$pre = $ch.Substring(0, $m.Index)
$post = $ch.Substring($m.Index + $m.Length)
$updated = $pre + $freshUnreleased + $newSection + $post

Set-Content -Path $changelogPath -Value $updated -NoNewline

Write-Host "Updated CHANGELOG.md with version $Version"

if ($Tag) {
  git add CHANGELOG.md
  git commit -m "chore(release): $Version"
  git tag "v$Version"
  Write-Host "Committed and tagged v$Version. Use 'git push --follow-tags' to publish."
}