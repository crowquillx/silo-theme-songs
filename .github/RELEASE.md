Theme Songs v0.2.2

- Owner lock and marker errors now name the media folder, such as `owner lock in "/mnt/remote/media/tv/Show": permission denied`. Before, the log said only `openat .silo-theme-download.lock: permission denied`.
- Each run warns when **AnimeThemes libraries** or **Skip these libraries** names a library that does not exist in Silo. If none of the AnimeThemes libraries exist, the status page says AnimeThemes will not run. The setting still applies as written.
- When a media folder holds a symlink Silo did not list, the plugin reads that item's files again before deciding. Tools such as Shoko's VFS add episode symlinks during a long run. If the symlink is still unknown, the item is deferred to the next run instead of refused. Unknown symlinks are still never followed or written through.

Linux amd64 and arm64 binaries and checksums are attached.
