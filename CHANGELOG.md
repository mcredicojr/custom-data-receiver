# Changelog
All notable changes to this project will be documented in this file.


## [Unreleased]

### Planned / Next Steps
- **Dashboard improvements**
  - Show image thumbnails when available (`images.path` exists).
  - Add an `/events/{id}` detail view to show all `event_fields` (alert list, site name, etc.).
- **Image handling**
  - Verify optional image fetch worker pulls images from Rekor Agent (`/img/{uuid}.jpg`).
  - Add retry / error handling for failed downloads.
- **Data model**
  - Decide which `alert.*` fields (list, list_id, type, site_name) should become indexed columns vs. stay in `event_fields`.
- **Providers**
  - Plan for second provider integration (e.g. email parser or another webhook).


## [Unreleased]
### Added
- None

### Changed
- add next steps checklist under Unreleased

### Fixed
- None
## [0.2.0] - 2025-08-24
### Added
- rekor: alert-aware normalization, store alert metadata & image UUIDs, optional image fetch worker

### Changed
- None

### Fixed
- None

## [0.1.0] - 2025-08-23
### Added
- Project scaffold, CI, and version bump script.


- add /events API and dashboard page### Changed
- None

### Fixed
- None

