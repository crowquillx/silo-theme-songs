Theme Songs v0.1.10

Series with a `Specials` folder, the Sonarr and Jellyfin name for season 0, were refused as an unsupported layout. On a test library this blocked 70 of 876 series. `Specials` now counts as season 0.

Library reads are four times faster. The plugin now sends Silo up to eight requests per second instead of two, which stays under Silo's standard API key limit even when both theme plugins share a key. A preview of 876 series takes about nine minutes.

The README notes that Silo runs every plugin task when it starts.

Linux amd64 and arm64 binaries and checksums are attached.
