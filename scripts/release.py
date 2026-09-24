#!/usr/bin/env python3
"""Build standalone plugin binaries and the Silo repository index."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import os
import tarfile


def main():
    manifest = json.loads(Path('manifest.json').read_text())
    version = manifest['version']
    repo = manifest['presentation']['source_url']
    dist = Path('dist')
    dist.mkdir(exist_ok=True)
    binaries = {}
    for arch in ('amd64', 'arm64'):
        path = dist / f'plugin-linux-{arch}'
        env = dict(os.environ, GOOS='linux', GOARCH=arch, CGO_ENABLED='0', GOWORK='off')
        subprocess.run(['go', 'build', '-trimpath', '-ldflags', f'-s -w -X main.version={version}', '-o', str(path), '.'], env=env, check=True)
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        binaries[f'linux/{arch}'] = {'url': f'{repo}/releases/download/v{version}/{path.name}', 'checksum': digest}
        package_manifest = dict(manifest, checksum=digest)
        manifest_path = dist / f'manifest-linux-{arch}.json'
        manifest_path.write_text(json.dumps(package_manifest, indent=2) + '\n')
        package_checksums = dist / f'checksums-linux-{arch}.txt'
        package_checksums.write_text(f'{digest}  plugin\n{hashlib.sha256(manifest_path.read_bytes()).hexdigest()}  manifest.json\n')
        with tarfile.open(dist / f'plugin-linux-{arch}.tar.gz', 'w:gz') as archive:
            archive.add(path, arcname='plugin')
            archive.add(manifest_path, arcname='manifest.json')
            archive.add(package_checksums, arcname='checksums.txt')
            for doc in ('LICENSE', 'README.md', 'THIRD_PARTY_NOTICES.md', 'config.example.json'):
                archive.add(doc, arcname=doc)
            archive.add('docs/compatibility.json', arcname='docs/compatibility.json')
            archive.add('docs/validation.md', arcname='docs/validation.md')
    manifest['checksum'] = binaries['linux/amd64']['checksum']
    (dist / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (dist / 'repository.json').write_text(json.dumps({'plugins': [{'manifest': manifest, 'repo_url': repo, 'binaries': binaries}]}, indent=2) + '\n')
    for doc in ('LICENSE', 'README.md', 'THIRD_PARTY_NOTICES.md', 'config.example.json'):
        shutil.copyfile(doc, dist / doc)
    files = sorted(p for p in dist.iterdir() if p.is_file() and p.name != 'checksums.txt')
    (dist / 'checksums.txt').write_text(''.join(f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n' for p in files))


if __name__ == '__main__':
    main()
