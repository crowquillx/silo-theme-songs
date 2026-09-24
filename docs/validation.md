# Compatibility checks

Validated on 2026-09-24 against the server and SDK commits recorded in [compatibility.json](compatibility.json), using a disposable PostgreSQL database and generated video fixtures.

- Both raw plugin binaries installed through the native upload API. Configuration, admin status routes, RuntimeHost access and scheduled tasks worked.
- An unscoped administrator API key with explicit primary-profile context read catalog membership and file paths. A scoped library-read key received authorization denials on required routes.
- A direct HTTPS audio fixture passed the downloader and ffprobe checks. The shared writer published it without replacement, and the real scanner assigned it to the expected dedicated movie owner with two versions.
- Separate season 2 and season 3 directories received distinct theme files. Their series root retained a separate fallback. Native and Jellyfin theme sets returned the exact expected owner and title.
- Native playback grants returned audio. Jellyfin theme audio returned HTTP 206 for byte-range requests. The tested host returned HTTP 404 for a synthetic theme item's `PlaybackInfo` request; direct theme audio playback worked.
- Autoscan established its marker handshake and discovered a new theme downloaded by the hosted scheduled task. Reconciliation confirmed its exact owner. A repeat run did not download it again.
- Focused server tests for the theme repository, scanner, native theme routes and Jellyfin theme routes passed against the disposable database. No server or SDK code changes were needed.

The Go race suite and vet cover inventory joins, unsafe layouts and mounts, locking, protected files, publication recovery, scan marker retries and resets, provider failures and mapping ambiguity. Provider HTTP and extractor tests use controlled fixtures.

The live ThemerrDB lookup for TMDB series 1399 resolved an exact YouTube URL. Extraction with yt-dlp 2026.8.19 returned `unavailable_media`; it did not switch sources or publish a partial file. Controlled extractor fixtures passed.

Live mapping snapshots were reachable. The AnimeThemes API/audio service was unavailable during validation, so a live AnimeThemes download could not be verified. Its mapping, selection and failure behavior passed fixture tests. The live direct-audio test verifies the shared downloader and writer, not AnimeThemes asset availability.

The experimental TelevisionTunes source is disabled. Its TLS certificate failed the live probe; TLS verification is never bypassed.
