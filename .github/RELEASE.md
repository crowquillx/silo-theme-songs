Theme Songs v0.1.13

Fixes saving the settings in Silo. Silo's settings form saves each text box as text, so entering library IDs or path mappings failed with `got string, want array` or `got string, want object`. The settings now accept text:

- **Selected library IDs**, **Excluded item IDs** and **Anime-managed library IDs** take IDs separated by commas or spaces, such as `3, 4`. A JSON array such as `["3", "4"]` also works.
- **Writable roots and path mappings** and **Provider settings** take a JSON object. Invalid JSON is rejected with an error that names the setting.

Linux amd64 and arm64 binaries and checksums are attached.
