# Changelog
All notable changes to this project will be documented in this file.

## [Unreleased]
- sqlite: enable WAL and busy_timeout(5s), and serialize connections to prevent SQLITE_BUSY under concurrent inserts.

## [Unreleased]

## [0.3.0] - 2025-08-24
### Added
- Support for Rekor image fetching from police.openalpr.com using `{agent_uid}` and `${REKOR_API_KEY}` expansion.
- Webhook handler now stores `agent_uid` and captures inline images (base64 or URL) immediately.
- Config option `path_template` for images (default example uses `/img/{agent_uid}/{uuid}?api_key=${REKOR_API_KEY}`).



- rekor: image fetch worker, YAML env key support, debug endpoints### Added (planned for v0.3.0)
- Dashboard: image thumbnails column when `images.path` is present.
- Dashboard: link to `/events/{id}` detail page.
- Event details page: show normalized fields plus all `event_fields` keys/values.
- API: `GET /events/{id}` to fetch a single event and its fields/images.

### Changed (planned)
- UI: improved time formatting and auto-refresh cadence configurable.


- add next steps checklist under Unreleased### Fixed (planned)
- Better resilience on image worker retries and error messages.

### Tasks
- [ ] Add `/events/{id}` API (event + `event_fields` + `images`).
- [ ] Add `/events/{id}` HTML page with metadata table and thumbnails.
- [ ] Add thumbnails column to `/dashboard` (if `images.path` exists).
- [ ] Image worker: bump retry backoff + max attempts; surface errors in UI.
- [ ] Optional: toggle to show Occurred vs Created time in dashboard.

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
- rekor: image fetch worker, YAML env key support, debug endpoints

### Changed
- add next steps checklist under Unreleased


- add next steps checklist under Unreleased### Fixed
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

