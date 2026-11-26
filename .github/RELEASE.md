Theme Songs v0.3.2

Theme Songs runs for new items after library scans again, and long runs are no longer stopped partway through.

- Silo builds from 18 November 2026 on send plugins only the events in the plugin SDK's event catalog. Theme Songs listened for Silo's older `scan_complete` event, which those builds no longer send, so it never ran after a scan. It now listens for `catalog.library.changed`, and still accepts `scan_complete` from older Silo versions. Changes that add no items, such as metadata edits, don't start a run. Thanks to @ArmoiraglatZ (#9).
- Silo stops a plugin after 30 minutes without calls between the two, and Theme Songs' downloads don't count, so a full run of a large library could be stopped partway through. While a run lasts, Theme Songs now calls Silo every 5 minutes, so Silo leaves it running. Thanks to @ArmoiraglatZ (#10).

Nothing needs to be changed after upgrading. Items a stopped run didn't reach are picked up by the next full run. For a run right away, choose **Download now** on the Theme Songs page.

Linux amd64 and arm64 binaries and checksums are attached.
