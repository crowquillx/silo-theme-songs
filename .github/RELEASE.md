Theme Songs v0.1.9

YouTube themes now work on Silo's Docker image without installing anything. When no yt-dlp or JavaScript runtime is on PATH, the first YouTube download installs the official yt-dlp build and Deno 2.9.8 in the plugin's state directory. The plugin verifies the yt-dlp release signature and the Deno checksum, and it checks for a new yt-dlp release once a day or an hour after an extraction failure. Set `"manage_tools": false` in Provider settings to opt out.

Setup needs only an API key. The Silo URL, profile, state directory and writable roots now have defaults. Without library IDs, Preview lists your movie and series libraries with their IDs. The README install steps now cover the server restart Silo needs before plugin tasks appear and the read-only media mount in Silo's Docker Compose file.

Deno 2 runtimes were rejected because their version line changed format; they are accepted again. Removed or private YouTube videos now report "YouTube video is unavailable".

Linux amd64 and arm64 binaries and checksums are attached.
