Theme Songs v0.3.1

Movies and series get their themes from ThemerrDB again, and series libraries with multi-episode files are read.

- ThemerrDB lookups now use `themerr.lizardbyte.dev`, where ThemerrDB is published now. The old address at `app.lizardbyte.dev/ThemerrDB` answers every request with HTTP 404, so 0.3.0 and earlier reported every movie and series as `provider themerrdb: absent` and downloaded no ThemerrDB themes. Thanks to @ALMN1905 (#6).
- A file that covers several episodes of one season, such as `S01E01-E03`, no longer stops the whole library with "ambiguous episode file membership". The file counts once, under its lowest episode. A file that Silo lists under two different seasons is still refused. Thanks to @ALMN1905 (#7).

Nothing needs to be cleared after upgrading. The next run looks up the items that were reported as absent. For a run right away, choose **Download now** on the Theme Songs page.

Linux amd64 and arm64 binaries and checksums are attached.
