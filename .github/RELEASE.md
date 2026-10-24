Theme Songs v0.1.14

Fixes v0.1.13, which exited at startup with `plugin config schema "settings" has invalid json_schema`. The Silo plugin SDK requires each setting to declare a single JSON type, so the settings that accept either text or JSON now declare their two forms with `anyOf`. Update from v0.1.13 to this release.

As in v0.1.13, the list settings take IDs separated by commas or spaces, such as `3, 4`, or a JSON array. **Writable roots and path mappings** and **Provider settings** take a JSON object.

Linux amd64 and arm64 binaries and checksums are attached.
