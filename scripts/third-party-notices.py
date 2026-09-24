#!/usr/bin/env python3
"""Collect dependency license texts from `go list -m -json all`."""
import json
from pathlib import Path
import subprocess


def main():
    subprocess.run(['go', 'mod', 'download', 'all'], check=True)
    remaining = subprocess.check_output(['go', 'list', '-m', '-json', 'all'], text=True)
    decoder = json.JSONDecoder()
    sections = [
        '# Third-party notices\n\n'
        'This release uses the modules below. Each section preserves the license '
        'and notice files distributed with its pinned Go module.\n'
    ]
    while remaining.strip():
        remaining = remaining.lstrip()
        module, end = decoder.raw_decode(remaining)
        remaining = remaining[end:]
        if module.get('Main'):
            continue
        root = Path(module['Dir'])
        files = sorted(p for p in root.iterdir() if p.is_file()
                       and p.name.lower().startswith(('license', 'copying', 'notice')))
        if not files:
            raise SystemExit(f"No license found for {module['Path']}")
        sections.append(f"\n## {module['Path']} {module['Version']}\n")
        for path in files:
            sections.append(f'\n{path.name}\n\n```text\n{path.read_text().rstrip()}\n```\n')
    go_root = Path(subprocess.check_output(['go', 'env', 'GOROOT'], text=True).strip())
    sections.append('\n## Go standard library\n\n```text\n'
                    + (go_root / 'LICENSE').read_text().rstrip() + '\n```\n')
    rendered = ''.join(sections)
    Path('THIRD_PARTY_NOTICES.md').write_text(
        '\n'.join(line.rstrip() for line in rendered.splitlines()) + '\n')


if __name__ == '__main__':
    main()
