Theme Songs v0.1.8

Fixes tasks that failed on real libraries with "Silo read rate limit exceeds remaining request budget". Silo allows each scheduled task call 10 seconds, and reading a library takes longer. Preview, Download and Reconcile now continue in the background after that call returns; the status page shows the result. A second trigger joins the task already running.

FFmpeg and ffprobe now default to Silo's Docker paths under `/usr/lib/jellyfin-ffmpeg`, falling back to PATH. Saved `ffmpeg` and `ffprobe` settings also find Silo's copy when PATH has none.

Media folders whose catalog video files are symlinks, as in debrid and rclone libraries, are no longer refused. Other symlinks are still refused, and the message names the file.

Linux amd64 and arm64 binaries and checksums are attached.
