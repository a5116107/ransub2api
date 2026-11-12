#!/usr/bin/env python3
"""Split GoReleaser builds across runners without requiring GoReleaser Pro."""
import argparse
import hashlib
import itertools
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import zipfile
from datetime import datetime, timezone

import yaml
from github_api import github
from urllib.parse import quote

FULL_CONFIG = Path('.goreleaser.yaml')
SIMPLE_CONFIG = Path('.goreleaser.simple.yaml')
VERSION_FILE = Path('backend/cmd/server/VERSION')
VERSION_RE = re.compile(r'\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?')
CUSTOM_STABLE_RE = re.compile(r'\d+\.\d+\.\d+-ransub\.[1-9]\d*')


def config(simple=False):
    return yaml.safe_load((SIMPLE_CONFIG if simple else FULL_CONFIG).read_text(encoding='utf-8'))


def targets(simple=False):
    build = config()['builds'][0]
    result = []
    for goos, goarch in itertools.product(build['goos'], build['goarch']):
        item = {'goos': goos, 'goarch': goarch}
        if any(all(item.get(k) == v for k, v in rule.items()) for rule in build.get('ignore', [])):
            continue
        if not simple or item == {'goos': 'linux', 'goarch': 'amd64'}:
            result.append(item)
    if not result:
        raise ValueError('empty release target matrix')
    return result


def archive_name(version, target):
    if not VERSION_RE.fullmatch(version) or target not in targets():
        raise ValueError('invalid release version or target')
    suffix = 'zip' if target['goos'] == 'windows' else 'tar.gz'
    return f"sub2api_{version}_{target['goos']}_{target['goarch']}.{suffix}"


def sha256(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def plan(args):
    sha = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True, encoding='utf-8').strip()
    if args.dry_run:
        version = VERSION_FILE.read_text(encoding='utf-8').strip()
        tag = 'v' + version
    else:
        tag = args.ref
        version = tag.removeprefix('v')
        if not tag.startswith('v') or not VERSION_RE.fullmatch(version):
            raise ValueError('publishing requires a v-prefixed release version tag')
        tagged_sha = subprocess.check_output(['git', 'rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}'], text=True, encoding='utf-8').strip()
        if sha != tagged_sha:
            raise ValueError('checkout does not match the selected release tag')
    if not VERSION_RE.fullmatch(version):
        raise ValueError('invalid VERSION')
    VERSION_FILE.write_text(version + '\n')
    result = {'sha': sha, 'tag': tag, 'version': version,
              'prerelease': str('-' in version and not CUSTOM_STABLE_RE.fullmatch(version)).lower(),
              'owner_lower': os.environ.get('GITHUB_REPOSITORY_OWNER', '').lower(),
              'simple': str(args.simple).lower(), 'dry_run': str(args.dry_run).lower(),
              'date': datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'),
              'matrix': json.dumps({'include': targets(args.simple)}, separators=(',', ':'))}
    with Path(os.environ['GITHUB_OUTPUT']).open('a') as output:
        for key, value in result.items():
            output.write(f'{key}={value}\n')


def generate_config(args):
    data = config(args.simple if args.mode == 'publish' else False)
    data['snapshot'] = {'version_template': '{{ .Env.RELEASE_VERSION }}'}
    data['dockers'] = []
    data['docker_manifests'] = []
    if CUSTOM_STABLE_RE.fullmatch(os.environ.get('RELEASE_VERSION', '')):
        data['release']['prerelease'] = False
    if args.mode == 'build':
        target = {'goos': args.goos, 'goarch': args.goarch}
        if target not in targets():
            raise ValueError('unsupported build target')
        for build in data['builds']:
            build['goos'], build['goarch'], build['ignore'] = [args.goos], [args.goarch], []
            build['ldflags'] = [re.sub(r'{{\s*\.Date\s*}}', '{{ .Env.RELEASE_DATE }}', flag)
                                for flag in build.get('ldflags', [])]
    else:
        # Artifacts are supplied through the OSS extra_files mechanism. No build
        # is repeated on the publishing runner, and release templates stay intact.
        data['before'] = {'hooks': []}
        data['builds'] = [{'id': 'sub2api', 'skip': True}]
        data['archives'] = []
        extra = [{'glob': 'release-input/sub2api_*.tar.gz'}, {'glob': 'release-input/sub2api_*.zip'}]
        if args.simple:
            data['checksum'] = {'disable': True}
        else:
            data['release']['extra_files'] = extra
            data['checksum'] = {'name_template': 'checksums.txt', 'algorithm': 'sha256', 'extra_files': extra}
    Path(args.output).write_text(yaml.safe_dump(data, sort_keys=False, allow_unicode=True), encoding='utf-8')


def collect(args):
    target = {'goos': args.goos, 'goarch': args.goarch}
    name = archive_name(args.version, target)
    source = Path('dist') / name
    checksums = {line.split()[1].lstrip('*'): line.split()[0] for line in Path('dist/checksums.txt').read_text(encoding='utf-8').splitlines()}
    digest = sha256(source)
    if checksums.get(name) != digest:
        raise ValueError('archive does not match the build checksum')
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, output / name)
    manifest = {'sha': args.sha, 'version': args.version, 'target': target, 'archive': name, 'sha256': digest}
    (output / f"manifest-{args.goos}-{args.goarch}.json").write_text(json.dumps(manifest) + '\n')


def verify(args):
    directory = Path(args.input)
    expected = set()
    for target in targets(args.simple):
        name = archive_name(args.version, target)
        manifest_name = f"manifest-{target['goos']}-{target['goarch']}.json"
        expected.update((name, manifest_name))
        manifest = json.loads((directory / manifest_name).read_text(encoding='utf-8'))
        if manifest != {'sha': args.sha, 'version': args.version, 'target': target,
                        'archive': name, 'sha256': sha256(directory / name)}:
            raise ValueError(f'build provenance or checksum mismatch: {name}')
    if {p.name for p in directory.iterdir()} != expected:
        raise ValueError('missing or unexpected release artifacts')


def contexts(args):
    verify(args)
    for target in targets(args.simple):
        if target['goos'] != 'linux':
            continue
        dest = Path(args.output) / target['goarch']
        dest.mkdir(parents=True, exist_ok=True)
        with tarfile.open(Path(args.input) / archive_name(args.version, target), 'r:gz') as archive:
            members = [member for member in archive.getmembers() if member.name in ('sub2api', './sub2api')]
            if len(members) != 1 or not members[0].isfile():
                raise ValueError('archive must contain one regular sub2api binary')
            with archive.extractfile(members[0]) as source, (dest / 'sub2api').open('wb') as output:
                shutil.copyfileobj(source, output)
        (dest / 'sub2api').chmod(0o755)
        shutil.copy2('Dockerfile.goreleaser', dest / 'Dockerfile')
        (dest / 'deploy').mkdir(exist_ok=True)
        shutil.copy2('deploy/docker-entrypoint.sh', dest / 'deploy/docker-entrypoint.sh')
        shutil.copytree('backend/resources', dest / 'backend/resources', dirs_exist_ok=True)
        shutil.copy2('THIRD_PARTY_NOTICES.md', dest / 'THIRD_PARTY_NOTICES.md')
        (dest / 'LICENSES').mkdir(exist_ok=True)
        shutil.copy2('LICENSES/BEpusdt-GPL-3.0.txt', dest / 'LICENSES/BEpusdt-GPL-3.0.txt')


def local_build(args):
    """Build the same release artifacts locally without starting GitHub CI."""
    if subprocess.check_output(['git', 'status', '--porcelain', '--untracked-files=all', '--', '.', ':!backend/internal/service/data'], text=True, encoding='utf-8').strip():
        raise ValueError('commit the verified source before building a release')
    version = VERSION_FILE.read_text(encoding='utf-8').strip()
    if not CUSTOM_STABLE_RE.fullmatch(version):
        raise ValueError('local custom releases require VERSION X.Y.Z-ransub.N')
    sha = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True, encoding='utf-8').strip()
    output = Path(args.output).resolve()
    inputs = output / 'input'
    inputs.mkdir(parents=True, exist_ok=True)
    if any(inputs.iterdir()):
        raise ValueError('release input directory is not empty; preserve or remove the previous artifacts explicitly')
    pnpm = shutil.which('pnpm.cmd') or shutil.which('pnpm')
    if not pnpm:
        raise ValueError('pnpm is required to build the matching embedded frontend')
    subprocess.run([pnpm, 'run', 'build'], cwd='frontend', check=True)
    date = datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    files = subprocess.check_output(['git', 'ls-files', 'LICENSE*', 'README*', 'deploy', 'backend/resources'], text=True, encoding='utf-8').splitlines()
    for target in targets(args.simple):
        binary = output / ('sub2api.exe' if target['goos'] == 'windows' else 'sub2api')
        env = dict(os.environ, CGO_ENABLED='0', GOOS=target['goos'], GOARCH=target['goarch'])
        flags = f'-s -w -X main.Version={version} -X main.Commit={sha} -X main.Date={date} -X main.BuildType=release'
        subprocess.run(['go', 'build', '-tags', 'embed', '-trimpath', '-ldflags', flags, '-o', str(binary), './cmd/server'], cwd='backend', env=env, check=True)
        name = archive_name(version, target)
        archive = inputs / name
        if target['goos'] == 'windows':
            with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as dest:
                dest.write(binary, 'sub2api.exe')
                for file in files:
                    dest.write(file, file)
        else:
            with tarfile.open(archive, 'w:gz') as dest:
                info = dest.gettarinfo(binary, 'sub2api')
                info.mode = 0o755
                with binary.open('rb') as source:
                    dest.addfile(info, source)
                for file in files:
                    dest.add(file, file, recursive=False)
        manifest = {'sha': sha, 'version': version, 'target': target, 'archive': name, 'sha256': sha256(archive)}
        (inputs / f"manifest-{target['goos']}-{target['goarch']}.json").write_text(json.dumps(manifest) + '\n', encoding='utf-8')
        binary.unlink()
    verify(argparse.Namespace(input=str(inputs), version=version, sha=sha, simple=args.simple))
    checksums = ''.join(f'{sha256(file)}  {file.name}\n' for file in sorted(inputs.glob('sub2api_*')))
    (output / 'checksums.txt').write_text(checksums, encoding='utf-8')
    provenance = {'repository': 'a5116107/ransub2api', 'version': version, 'sha': sha, 'built_at': date,
                  'simple': args.simple, 'targets': targets(args.simple), 'upstream': 'ranxi2001/sub2api',
                  'build_type': 'release'}
    (output / 'release-manifest.json').write_text(json.dumps(provenance, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(provenance))


def local_publish(args):
    """Publish verified local archives; CI and deployment are not invoked."""
    output = Path(args.input).resolve()
    manifest = json.loads((output / 'release-manifest.json').read_text(encoding='utf-8'))
    if manifest['repository'] != 'a5116107/ransub2api' or not CUSTOM_STABLE_RE.fullmatch(manifest['version']):
        raise ValueError('unexpected release repository or version')
    tag = 'v' + manifest['version']
    tagged = subprocess.check_output(['git', 'rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}'], text=True, encoding='utf-8').strip()
    if tagged != manifest['sha']:
        raise ValueError('release tag does not identify the built source')
    remote_refs = subprocess.check_output(['git', 'ls-remote', 'origin', f'refs/tags/{tag}', f'refs/tags/{tag}^{{}}'], text=True, encoding='utf-8')
    refs = {line.split()[1]: line.split()[0] for line in remote_refs.splitlines()}
    if refs.get(f'refs/tags/{tag}^{{}}', refs.get(f'refs/tags/{tag}')) != tagged:
        raise ValueError('remote release tag does not identify the built source')
    verify(argparse.Namespace(input=str(output / 'input'), version=manifest['version'], sha=tagged, simple=manifest['simple']))
    checksums = ''.join(f'{sha256(file)}  {file.name}\n' for file in sorted((output / 'input').glob('sub2api_*')))
    if (output / 'checksums.txt').read_text(encoding='utf-8') != checksums:
        raise ValueError('release checksums do not match the verified archives')
    assets = sorted((output / 'input').iterdir()) + [output / 'checksums.txt', output / 'release-manifest.json']
    repo = manifest['repository']
    releases = github('GET', f'repos/{repo}/releases?per_page=100')
    draft = next((release for release in releases if release['tag_name'] == tag), None)
    if draft is not None and not draft['draft']:
        raise ValueError('selected release is already published; its assets must remain immutable')
    if draft is None:
        draft = github('POST', f'repos/{repo}/releases', {'tag_name': tag, 'name': f'RanSub2API {manifest["version"]}',
                       'body': Path(args.notes).read_text(encoding='utf-8'), 'draft': True, 'prerelease': False})
    release_id = draft['id']
    existing = {asset['name']: asset for asset in draft.get('assets', [])}
    for asset in assets:
        if asset.name in existing:
            uploaded = existing[asset.name]
            if uploaded.get('size') != asset.stat().st_size or uploaded.get('digest') != 'sha256:' + sha256(asset):
                raise ValueError('existing draft asset has different content; draft retained')
            continue
        github('POST', f'https://uploads.github.com/repos/{repo}/releases/{release_id}/assets?name={quote(asset.name)}', upload=asset)
    remote = github('GET', f'repos/{repo}/releases/{release_id}')
    by_name = {asset['name']: asset for asset in remote['assets']}
    for asset in assets:
        uploaded = by_name.get(asset.name, {})
        if uploaded.get('size') != asset.stat().st_size or uploaded.get('digest') != 'sha256:' + sha256(asset):
            raise ValueError('uploaded release asset verification failed; draft retained')
    github('PATCH', f'repos/{repo}/releases/{release_id}', {'draft': False, 'prerelease': False, 'make_latest': 'true'})
    print(json.dumps({'repository': repo, 'tag': tag, 'sha': tagged, 'published': True}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    p = commands.add_parser('plan')
    p.add_argument('--ref', required=True)
    p.add_argument('--simple', action='store_true')
    p.add_argument('--dry-run', action='store_true')
    p.set_defaults(run=plan)
    p = commands.add_parser('config')
    p.add_argument('mode', choices=['build', 'publish'])
    p.add_argument('--goos')
    p.add_argument('--goarch')
    p.add_argument('--simple', action='store_true')
    p.add_argument('--output', required=True)
    p.set_defaults(run=generate_config)
    p = commands.add_parser('collect')
    for arg in ('version', 'sha', 'goos', 'goarch', 'output'):
        p.add_argument('--' + arg, required=True)
    p.set_defaults(run=collect)
    for command, handler in [('verify', verify), ('contexts', contexts)]:
        p = commands.add_parser(command)
        for arg in ('version', 'sha', 'input'):
            p.add_argument('--' + arg, required=True)
        p.add_argument('--simple', action='store_true')
        if command == 'contexts':
            p.add_argument('--output', required=True)
        p.set_defaults(run=handler)
    p = commands.add_parser('local-build')
    p.add_argument('--output', required=True)
    p.add_argument('--simple', action='store_true', help='build the Linux amd64 archive only')
    p.set_defaults(run=local_build)
    p = commands.add_parser('local-publish')
    p.add_argument('--input', required=True)
    p.add_argument('--notes', required=True)
    p.set_defaults(run=local_publish)
    p = commands.add_parser('local-rehearse')
    for arg in ('baseline', 'candidate', 'candidate-sha', 'scratch', 'output'):
        p.add_argument('--' + arg, required=True)
    p.add_argument('--runtime-image', default='python:3.11-slim')
    p.add_argument('--postgres-image', default='postgres:17-alpine')
    p.add_argument('--redis-image', default='redis:7-alpine')
    from release_rehearsal import local_rehearse
    p.set_defaults(run=lambda args: local_rehearse(args, verify, sha256))
    args = parser.parse_args()
    args.run(args)


if __name__ == '__main__':
    main()
