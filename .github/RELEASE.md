Theme Songs v0.2.0

AnimeThemes is now part of Theme Songs, and setup is now install, paste an API key and save.

- Every movie and series library is used by default. Anime series that AnimeThemes knows get openings and endings per season; everything else uses ThemerrDB.
- Theme Songs runs on its own: once a day, after Silo scans a library with new items, and when Silo starts. Runs download by default; **Dry run** lists them instead.
- After saving a theme, Theme Songs asks Silo to scan that folder, so the Autoscan source, manual scans and the Preview and Reconcile tasks are gone.
- The Theme Songs page has library checkboxes and Download and Preview buttons, and it says when media is mounted read-only.
- The settings form has sections, descriptions and switches for the common options.
- New themes and `theme-music` folders follow Silo's umask, as Jellyfin's theme plugins do, instead of being readable only by Silo's user.
- AnimeThemes downloads work again: the AnimeThemes API stopped putting the file extension in `filename`.

If the AnimeThemes plugin is installed, uninstall it. Until then, Theme Songs leaves anime libraries to it. Afterwards, Theme Songs copies its state and keeps managing its themes. Re-enter any AnimeThemes manual mappings under **AnimeThemes settings (JSON)**.

Linux amd64 and arm64 binaries and checksums are attached.
