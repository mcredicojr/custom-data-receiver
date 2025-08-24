param(
  [Parameter(Mandatory=$true)][string]$Version
)

# Update changelog and create tag
pwsh ./tools/bump-version.ps1 -Version $Version -Tag

# Push commit + tag
git push --follow-tags
Write-Host "Released v$Version"
