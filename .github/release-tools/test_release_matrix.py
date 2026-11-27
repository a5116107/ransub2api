import argparse
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tarfile
import tempfile
import unittest
import zipfile
from unittest.mock import patch

import yaml

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('release_matrix', Path(__file__).with_name('release_matrix.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseMatrixTest(unittest.TestCase):
    def test_custom_stable_image_publishes_revision_and_latest(self):
        fake_bin = Path('bin')
        fake_bin.mkdir()
        docker = fake_bin / 'docker'
        docker.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$DOCKER_LOG"\n')
        docker.chmod(0o755)
        env = {**os.environ, 'PATH': str(fake_bin.resolve()) + os.pathsep + os.environ['PATH'],
               'DOCKER_LOG': str(Path('docker.log').resolve()), 'RUNNER_TEMP': self.temp.name,
               'RELEASE_VERSION': '2.19.2-ransub.1', 'RELEASE_SHA': 'a' * 40,
               'GITHUB_REPOSITORY': 'a5116107/ransub2api', 'DRY_RUN': 'false',
               'SIMPLE_RELEASE': 'true', 'DOCKERHUB_USERNAME': 'skip'}
        subprocess.run(['bash', str(ROOT / '.github/release-tools/release-images.sh')], env=env, check=True)
        log = Path('docker.log').read_text(encoding='utf-8')
        self.assertIn('ghcr.io/a5116107/ransub2api:2.19.2-ransub.1', log)
        self.assertIn('ghcr.io/a5116107/ransub2api:latest', log)

    def test_automatic_image_is_not_latest_before_release_publication(self):
        fake_bin = Path('bin')
        fake_bin.mkdir()
        docker = fake_bin / 'docker'
        docker.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$DOCKER_LOG"\n')
        docker.chmod(0o755)
        env = {**os.environ, 'PATH': str(fake_bin.resolve()) + os.pathsep + os.environ['PATH'],
               'DOCKER_LOG': str(Path('docker.log').resolve()), 'RUNNER_TEMP': self.temp.name,
               'RELEASE_VERSION': '9.8.7-ransub.3', 'RELEASE_SHA': 'a' * 40,
               'GITHUB_REPOSITORY': release.CUSTOM_REPOSITORY, 'DRY_RUN': 'false',
               'SIMPLE_RELEASE': 'true', 'DOCKERHUB_USERNAME': 'skip', 'PUBLISH_LATEST': 'false'}
        subprocess.run(['bash', str(ROOT / '.github/release-tools/release-images.sh')], env=env, check=True)
        log = Path('docker.log').read_text()
        self.assertIn('--push', log)
        self.assertIn(':9.8.7-ransub.3', log)
        self.assertNotIn(':latest', log)

    def test_production_release_is_automatic_and_prebuilt_mode_stays_manual(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/release.yml').read_text(encoding='utf-8'))
        trigger = workflow.get('on', workflow.get(True))
        self.assertEqual(set(trigger), {'workflow_dispatch', 'push', 'schedule'})
        self.assertEqual(trigger['push']['branches'], ['production'])
        self.assertNotIn('tags', trigger['push'])
        self.assertEqual(trigger['schedule'], [{'cron': '*/15 * * * *'}])
        self.assertFalse(workflow['concurrency']['cancel-in-progress'])
        automatic = workflow['jobs']['publish-production']
        self.assertIn("github.event_name != 'workflow_dispatch'", automatic['if'])
        self.assertIn("github.repository == 'a5116107/ransub2api'", automatic['if'])
        self.assertIn("github.event_name == 'workflow_dispatch'", workflow['jobs']['prepare']['if'])
        steps = automatic['steps']
        commands = '\n'.join(step.get('run', '') for step in steps)
        self.assertNotRegex(commands, r'go test|pytest|unittest|pnpm (test|lint)|ssh |systemctl|compose up')
        image = next(i for i, step in enumerate(steps) if step.get('name') == 'Publish verified production runtime image')
        archive = next(i for i, step in enumerate(steps) if step.get('name') == 'Publish checksummed archives as latest custom stable release')
        latest = next(i for i, step in enumerate(steps) if step.get('name') == 'Promote the published stable image')
        self.assertEqual(steps[image]['env']['PUBLISH_LATEST'], 'false')
        self.assertLess(image, archive)
        self.assertLess(archive, latest)
        identity = next(step for step in steps if step.get('name') == 'Verify packaged executable build identity')
        self.assertIn('-version 2>&1', identity['run'])
        self.assertIn('"$RELEASE_SHA"', identity['run'])
        self.assertIn('prebuilt_only', trigger['workflow_dispatch']['inputs'])
        self.assertIn('prebuilt_only != true', workflow['jobs']['prepare']['if'])
        steps = workflow['jobs']['publish-prebuilt-image']['steps']
        self.assertFalse(any('go build' in step.get('run', '') or 'pnpm' in step.get('run', '') for step in steps))

    def test_custom_stable_version_is_not_hidden_as_prerelease(self):
        self.assertTrue(release.CUSTOM_STABLE_RE.fullmatch('2.19.2-ransub.1'))
        self.assertFalse(release.CUSTOM_STABLE_RE.fullmatch('2.19.2-beta.1'))
        with patch.dict(os.environ, {'RELEASE_VERSION': '2.19.2-ransub.1'}):
            release.generate_config(argparse.Namespace(mode='publish', simple=False, output='publisher.yaml'))
        self.assertIs(yaml.safe_load(Path('publisher.yaml').read_text(encoding='utf-8'))['release']['prerelease'], False)

    def test_local_release_rejects_uncommitted_source_before_build(self):
        with patch.object(subprocess, 'check_output', return_value=' M source.go\n'), patch.object(subprocess, 'run') as run:
            with self.assertRaisesRegex(ValueError, 'commit the verified source'):
                release.local_build(argparse.Namespace(output='release', simple=True))
            run.assert_not_called()

    def test_local_build_uses_selected_version_and_bypasses_package_test_hooks(self):
        def build(command, **kwargs):
            if command[:4] == ['pnpm', 'exec', 'vite', 'build']:
                frontend = Path(command[command.index('--outDir') + 1])
                frontend.mkdir()
                (frontend / 'index.html').write_text('current frontend')
            if command[:2] == ['go', 'build']:
                Path(command[command.index('-o') + 1]).write_bytes(b'fixture binary')
            return subprocess.CompletedProcess(command, 0)

        with patch.object(subprocess, 'check_output', side_effect=['', 'a' * 40, '']), \
                patch.object(shutil, 'which', return_value='pnpm'), \
                patch.object(subprocess, 'run', side_effect=build) as command:
            release.local_build(argparse.Namespace(output='build', simple=True, version='9.8.7-ransub.3'))
        commands = [call.args[0] for call in command.call_args_list]
        self.assertEqual(commands[0], ['pnpm', 'exec', 'vue-tsc', '-b'])
        self.assertEqual(commands[1][:6], ['pnpm', 'exec', 'vite', 'build', '--config', 'vite.config.ts'])
        self.assertIn('--emptyOutDir', commands[1])
        self.assertIn('main.Version=9.8.7-ransub.3', commands[2][commands[2].index('-ldflags') + 1])
        release.verify(argparse.Namespace(input='build/input', version='9.8.7-ransub.3', sha='a' * 40, simple=True))

    def test_local_build_embeds_only_fresh_frontend_without_deleting_old_outputs(self):
        dist = Path('backend/internal/web/dist').resolve()
        dist.mkdir(parents=True)
        obsolete = dist / 'obsolete.js'
        obsolete.write_text('protected historical output')
        observed = {}

        def build(command, **kwargs):
            if command[:4] == ['pnpm', 'exec', 'vite', 'build']:
                fresh = Path(command[command.index('--outDir') + 1])
                fresh.mkdir()
                (fresh / 'index.html').write_text('new entry')
                (fresh / 'current.js').write_text('new code')
                observed['fresh'] = fresh
            if command[:2] == ['go', 'build']:
                overlay = Path(command[command.index('-overlay') + 1])
                replacement = json.loads(overlay.read_text())['Replace']
                self.assertEqual(replacement[str(obsolete)], '')
                self.assertEqual(Path(replacement[str(dist / 'current.js')]).read_text(), 'new code')
                self.assertEqual(Path(replacement[str(dist / 'index.html')]).read_text(), 'new entry')
                observed['overlay'] = overlay
                Path(command[command.index('-o') + 1]).write_bytes(b'fixture binary')
            return subprocess.CompletedProcess(command, 0)

        with patch.object(subprocess, 'check_output', side_effect=['', 'a' * 40, '']), \
                patch.object(shutil, 'which', return_value='pnpm'), \
                patch.object(subprocess, 'run', side_effect=build):
            release.local_build(argparse.Namespace(output='build', simple=True, version='9.8.7-ransub.3'))
        self.assertEqual(obsolete.read_text(), 'protected historical output')
        self.assertFalse(observed['fresh'].exists())
        self.assertFalse(observed['overlay'].exists())

    def test_verify_enforces_online_updater_binary_limit_for_every_target(self):
        args = self.fixture_artifacts()
        with patch.object(release, 'MAX_UPDATE_BINARY_BYTES', 7, create=True):
            release.verify(args)
        for target in release.targets(False):
            selected_dir = Path(f"verify-{target['goos']}-{target['goarch']}")
            selected_dir.mkdir()
            for name in (release.archive_name(args.version, target), f"manifest-{target['goos']}-{target['goarch']}.json"):
                shutil.copy2(Path(args.input) / name, selected_dir / name)
            with self.subTest(target=target), \
                    patch.object(release, 'targets', return_value=[target]), \
                    patch.object(release, 'MAX_UPDATE_BINARY_BYTES', 6, create=True):
                with self.assertRaisesRegex(ValueError, 'online updater binary limit'):
                    release.verify(argparse.Namespace(**{**vars(args), 'input': str(selected_dir)}))

    def test_verify_rejects_archive_larger_than_online_download_limit(self):
        args = self.fixture_artifacts(True)
        with patch.object(release, 'MAX_UPDATE_DOWNLOAD_BYTES', 1, create=True):
            with self.assertRaisesRegex(ValueError, 'online updater download limit'):
                release.verify(args)

    def test_verify_rejects_empty_duplicate_linked_or_missing_executable(self):
        args = self.fixture_artifacts()
        for target in release.targets(False):
            for kind in ('empty', 'duplicate', 'linked', 'missing'):
                with self.subTest(target=target, kind=kind):
                    directory = Path(f"invalid-{target['goos']}-{target['goarch']}-{kind}")
                    directory.mkdir()
                    name = release.archive_name(args.version, target)
                    archive_path = directory / name
                    if target['goos'] == 'windows':
                        with zipfile.ZipFile(archive_path, 'w') as archive:
                            entry = zipfile.ZipInfo('sub2api.exe' if kind != 'missing' else 'other')
                            entry.create_system = 3
                            entry.external_attr = (stat.S_IFLNK | 0o777) << 16 if kind == 'linked' else (stat.S_IFREG | 0o755) << 16
                            archive.writestr(entry, b'' if kind == 'empty' else b'fixture')
                            if kind == 'duplicate':
                                archive.writestr('./sub2api.exe', b'fixture')
                    else:
                        with tarfile.open(archive_path, 'w:gz') as archive:
                            entry = tarfile.TarInfo('sub2api' if kind != 'missing' else 'other')
                            entry.size = 0 if kind == 'empty' else 7
                            if kind == 'linked':
                                entry.type, entry.linkname, entry.size = tarfile.SYMTYPE, 'other', 0
                            archive.addfile(entry, io.BytesIO(b'fixture'))
                            if kind == 'duplicate':
                                duplicate = tarfile.TarInfo('./sub2api')
                                duplicate.size = 7
                                archive.addfile(duplicate, io.BytesIO(b'fixture'))
                    manifest = {'sha': args.sha, 'version': args.version, 'target': target, 'archive': name, 'sha256': release.sha256(archive_path)}
                    (directory / f"manifest-{target['goos']}-{target['goarch']}.json").write_text(json.dumps(manifest))
                    with patch.object(release, 'targets', return_value=[target]), self.assertRaises(ValueError):
                        release.verify(argparse.Namespace(**{**vars(args), 'input': str(directory)}))

    def test_local_build_releases_fresh_frontend_after_compiler_failure(self):
        observed = {}

        def build(command, **kwargs):
            if command[:4] == ['pnpm', 'exec', 'vite', 'build']:
                fresh = Path(command[command.index('--outDir') + 1])
                fresh.mkdir()
                (fresh / 'index.html').write_text('new entry')
                observed['fresh'] = fresh
            if command[:2] == ['go', 'build']:
                raise subprocess.CalledProcessError(1, command)
            return subprocess.CompletedProcess(command, 0)

        with patch.object(subprocess, 'check_output', side_effect=['', 'a' * 40, '']), \
                patch.object(shutil, 'which', return_value='pnpm'), \
                patch.object(subprocess, 'run', side_effect=build):
            with self.assertRaises(subprocess.CalledProcessError):
                release.local_build(argparse.Namespace(output='build', simple=True, version='9.8.7-ransub.3'))
        self.assertFalse(observed['fresh'].exists())
        self.assertEqual(list(Path('build/input').iterdir()), [])

    def test_local_publication_rejects_wrong_repository_before_upload(self):
        Path('release').mkdir()
        Path('release/release-manifest.json').write_text(json.dumps({'repository': 'ranxi2001/sub2api', 'version': '2.19.2-ransub.1'}))
        with patch.object(subprocess, 'run') as run:
            with self.assertRaisesRegex(ValueError, 'unexpected release repository'):
                release.local_publish(argparse.Namespace(input='release', notes='notes.md'))
            run.assert_not_called()

    def setUp(self):
        self.previous = Path.cwd()
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        os.chdir(self.temp.name)
        self.addCleanup(os.chdir, self.previous)
        for name in ('.goreleaser.yaml', '.goreleaser.simple.yaml'):
            shutil.copyfile(ROOT / name, name)
        Path('backend/cmd/server').mkdir(parents=True)
        release.VERSION_FILE.write_text('9.8.7\n')

    def fixture_artifacts(self, simple=False, version='9.8.7'):
        directory = Path('release-input')
        directory.mkdir()
        for target in release.targets(simple):
            name = release.archive_name(version, target)
            archive = directory / name
            if target['goos'] != 'windows':
                with tarfile.open(archive, 'w:gz') as out:
                    info = tarfile.TarInfo('sub2api')
                    info.size = 7
                    info.mode = 0o755
                    out.addfile(info, io.BytesIO(b'fixture'))
            else:
                with zipfile.ZipFile(archive, 'w') as out:
                    out.writestr('sub2api.exe', b'fixture')
            metadata = {'version': version, 'sha': 'a' * 40, 'target': target,
                        'archive': name, 'sha256': release.sha256(archive)}
            (directory / f"manifest-{target['goos']}-{target['goarch']}.json").write_text(json.dumps(metadata))
        return argparse.Namespace(input='release-input', version=version, sha='a' * 40, simple=simple, output='contexts')

    def production_history(self, changed=True):
        for command in (['init', '-q', '--initial-branch=production'],
                        ['config', 'user.name', 'Fixture'], ['config', 'user.email', 'fixture@example.invalid'],
                        ['add', '.'], ['commit', '-qm', 'Published source'],
                        ['tag', 'v9.8.7-ransub.2']):
            subprocess.run(['git', *command], check=True)
        Path('docs').mkdir()
        Path('docs/notes.md').write_text('Documentation change\n')
        if changed:
            Path('application.go').write_text('package fixture\n')
        subprocess.run(['git', 'add', '.'], check=True)
        subprocess.run(['git', 'commit', '-qm', 'Candidate source'], check=True)

    def automatic_plan(self):
        with patch.dict(os.environ, {'GITHUB_OUTPUT': 'outputs'}), \
                patch.object(release, 'automatic_source', return_value=('a' * 40, True)), \
                patch.object(release, 'github', return_value={'tag_name': 'v9.8.7-ransub.2'}):
            release.automatic_plan(argparse.Namespace())
        return dict(line.split('=', 1) for line in Path('outputs').read_text().splitlines())

    def test_automatic_plan_uses_real_source_diff_and_increments_revision(self):
        self.production_history()
        result = self.automatic_plan()
        self.assertEqual(result['publish'], 'true')
        self.assertEqual(result['tag'], 'v9.8.7-ransub.3')
        self.assertEqual(result['sha'], 'a' * 40)

    def test_automatic_plan_skips_documentation_only_changes(self):
        self.production_history(changed=False)
        result = self.automatic_plan()
        self.assertEqual(result['publish'], 'false')
        self.assertEqual(result['image_tag'], 'v9.8.7-ransub.2')

    def test_automatic_plan_skips_go_test_only_changes(self):
        self.production_history(changed=False)
        source = Path('backend/internal/handler/fixture_test.go')
        source.parent.mkdir(parents=True)
        source.write_text('package handler\n')
        subprocess.run(['git', 'add', '.'], check=True)
        subprocess.run(['git', 'commit', '-qm', 'Repair test lifecycle'], check=True)
        self.assertEqual(self.automatic_plan()['publish'], 'false')

    def test_automatic_plan_publishes_runtime_changes_alongside_go_tests(self):
        self.production_history(changed=False)
        source = Path('backend/internal/handler/fixture_test.go')
        source.parent.mkdir(parents=True)
        source.write_text('package handler\n')
        source.with_name('runtime.go').write_text('package handler\n')
        subprocess.run(['git', 'add', '.'], check=True)
        subprocess.run(['git', 'commit', '-qm', 'Change runtime and its tests'], check=True)
        self.assertEqual(self.automatic_plan()['publish'], 'true')

    def test_automatic_plan_keeps_embedded_and_dependency_changes(self):
        self.production_history(changed=False)
        for name in ('backend/internal/service/fixture_test.go.json', 'backend/migrations/999_fixture.sql',
                     'backend/go.sum', 'backend/resources/fixture.json'):
            with self.subTest(path=name):
                source = Path(name)
                source.parent.mkdir(parents=True, exist_ok=True)
                source.write_text('runtime input\n')
                subprocess.run(['git', 'add', '.'], check=True)
                subprocess.run(['git', 'commit', '-qm', 'Change application input'], check=True)
                self.assertEqual(self.automatic_plan()['publish'], 'true')
                Path('outputs').unlink()
                source.unlink()
                subprocess.run(['git', 'add', '.'], check=True)
                subprocess.run(['git', 'commit', '-qm', 'Restore application input'], check=True)

    def test_automatic_plan_reuses_unpublished_tag_at_exact_source(self):
        self.production_history()
        subprocess.run(['git', 'tag', 'v9.8.7-ransub.3'], check=True)
        self.assertEqual(self.automatic_plan()['tag'], 'v9.8.7-ransub.3')

    def test_automatic_plan_does_not_reuse_tag_at_other_source(self):
        self.production_history()
        subprocess.run(['git', 'tag', 'v9.8.7-ransub.3', 'HEAD^'], check=True)
        self.assertEqual(self.automatic_plan()['tag'], 'v9.8.7-ransub.4')

    def test_automatic_plan_preserves_new_upstream_version(self):
        self.production_history()
        release.VERSION_FILE.write_text('9.8.8\n')
        self.assertEqual(self.automatic_plan()['tag'], 'v9.8.8-ransub.1')

    def test_automatic_plan_rejects_downgrade_and_prerelease_source(self):
        self.production_history()
        for version in ('9.8.6', '9.8.8-rc.1'):
            with self.subTest(version=version):
                release.VERSION_FILE.write_text(version + '\n')
                with self.assertRaises(ValueError):
                    self.automatic_plan()

    def test_automatic_source_rejects_fork_and_feature_branch_before_git(self):
        for repo, ref in (('other/fork', 'refs/heads/production'),
                          (release.CUSTOM_REPOSITORY, 'refs/heads/feature'),
                          (release.CUSTOM_REPOSITORY, 'refs/tags/v9.8.7-ransub.3')):
            with self.subTest(repo=repo, ref=ref), \
                    patch.dict(os.environ, {'GITHUB_REPOSITORY': repo, 'GITHUB_REF': ref}), \
                    patch.object(subprocess, 'check_output') as command:
                with self.assertRaisesRegex(ValueError, 'owner production branch'):
                    release.automatic_source()
                command.assert_not_called()

    def test_automatic_source_detects_advanced_production(self):
        with patch.dict(os.environ, {'GITHUB_REPOSITORY': release.CUSTOM_REPOSITORY, 'GITHUB_REF': 'refs/heads/production'}), \
                patch.object(subprocess, 'check_output', side_effect=['a' * 40, 'b' * 40 + '\trefs/heads/production\n']):
            self.assertEqual(release.automatic_source(), ('a' * 40, False))

    def test_automatic_plan_skips_stale_job_without_reading_releases(self):
        with patch.dict(os.environ, {'GITHUB_OUTPUT': 'outputs'}), \
                patch.object(release, 'automatic_source', return_value=('a' * 40, False)), \
                patch.object(release, 'github') as api:
            release.automatic_plan(argparse.Namespace())
            api.assert_not_called()
        self.assertIn('publish=false', Path('outputs').read_text())

    def publication_fixture(self):
        built = self.fixture_artifacts(True, '9.8.7-ransub.3')
        output = Path('release')
        output.mkdir()
        shutil.move(built.input, output / 'input')
        manifest = {'repository': release.CUSTOM_REPOSITORY, 'version': built.version,
                    'sha': built.sha, 'simple': True}
        (output / 'release-manifest.json').write_text(json.dumps(manifest))
        checksums = ''.join(f'{release.sha256(file)}  {file.name}\n' for file in sorted((output / 'input').glob('sub2api_*')))
        (output / 'checksums.txt').write_text(checksums)
        Path('notes.md').write_text('Fixture release\n')
        return argparse.Namespace(input=str(output), notes='notes.md', automatic=True)

    def publication_api(self, corrupt_upload=False):
        assets = {}
        for file in [*Path('release/input').iterdir(), Path('release/checksums.txt'), Path('release/release-manifest.json')]:
            assets[file.name] = {'id': len(assets) + 1, 'name': file.name, 'size': file.stat().st_size,
                                 'digest': 'sha256:' + release.sha256(file)}
        draft = {'id': 123, 'tag_name': 'v9.8.7-ransub.3', 'draft': True,
                 'assets': [{**asset, 'digest': 'sha256:stale'} for asset in assets.values()]}

        def api(method, path, data=None, *, upload=None):
            if method == 'GET' and path.endswith('releases?per_page=100'):
                return [draft]
            if method == 'GET' and path.endswith('releases/123'):
                return {'assets': draft['assets'] if corrupt_upload else list(assets.values())}
            if method == 'GET' and path.endswith('releases/latest'):
                return {'tag_name': 'v9.8.7-ransub.2'}
            if method in ('POST', 'DELETE', 'PATCH'):
                return {}
            raise AssertionError((method, path))
        return api

    def test_automatic_publication_repairs_only_unpublished_draft_assets(self):
        args = self.publication_fixture()
        refs = 'a' * 40 + '\trefs/tags/v9.8.7-ransub.3^{}\n'
        with patch.object(subprocess, 'check_output', side_effect=['a' * 40, refs]), \
                patch.object(release, 'automatic_source', return_value=('a' * 40, True)), \
                patch.object(release, 'github', side_effect=self.publication_api()) as api:
            release.local_publish(args)
        self.assertEqual(sum(call.args[0] == 'DELETE' for call in api.call_args_list), 4)
        self.assertEqual(api.call_args_list[-1].args[0], 'PATCH')

    def test_manual_publication_preserves_draft_on_mismatch(self):
        args = self.publication_fixture()
        args.automatic = False
        refs = 'a' * 40 + '\trefs/tags/v9.8.7-ransub.3^{}\n'
        with patch.object(subprocess, 'check_output', side_effect=['a' * 40, refs]), \
                patch.object(release, 'github', side_effect=self.publication_api()) as api:
            with self.assertRaisesRegex(ValueError, 'different content'):
                release.local_publish(args)
        self.assertFalse(any(call.args[0] == 'DELETE' for call in api.call_args_list))

    def test_automatic_publication_never_promotes_stale_or_corrupt_upload(self):
        args = self.publication_fixture()
        refs = 'a' * 40 + '\trefs/tags/v9.8.7-ransub.3^{}\n'
        for corrupt in (False, True):
            with self.subTest(corrupt=corrupt), \
                    patch.object(subprocess, 'check_output', side_effect=['a' * 40, refs]), \
                    patch.object(release, 'automatic_source', return_value=('a' * 40, False)), \
                    patch.object(release, 'github', side_effect=self.publication_api(corrupt)) as api:
                with self.assertRaises(ValueError):
                    release.local_publish(args)
                self.assertFalse(any(call.args[0] == 'PATCH' for call in api.call_args_list))

    def test_published_assets_are_never_replaced_by_automatic_recovery(self):
        args = self.publication_fixture()
        refs = 'a' * 40 + '\trefs/tags/v9.8.7-ransub.3^{}\n'
        with patch.object(subprocess, 'check_output', side_effect=['a' * 40, refs]), \
                patch.object(release, 'github', return_value=[{'tag_name': 'v9.8.7-ransub.3', 'draft': False}]) as api:
            with self.assertRaisesRegex(ValueError, 'already published'):
                release.local_publish(args)
        self.assertTrue(all(call.args[0] == 'GET' for call in api.call_args_list))

    def test_automatic_publish_rejects_mismatched_artifacts_before_tag_push(self):
        args = self.publication_fixture()
        Path('release/input/manifest-linux-amd64.json').write_text('{}')
        with patch.object(release, 'automatic_source', return_value=('a' * 40, True)), \
                patch.object(subprocess, 'run') as command:
            with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
                release.automatic_publish(args)
            command.assert_not_called()

    def test_automatic_publish_pushes_only_matching_source_tag(self):
        args = self.publication_fixture()
        with patch.object(release, 'automatic_source', return_value=('a' * 40, True)), \
                patch.object(release, 'github', return_value={'tag_name': 'v9.8.7-ransub.2'}), \
                patch.object(subprocess, 'check_output', return_value='- Accepted change\n'), \
                patch.object(subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, stdout='a' * 40)) as command, \
                patch.object(release, 'local_publish') as publish:
            release.automatic_publish(args)
        self.assertEqual(command.call_args_list[-1].args[0], ['git', 'push', 'origin', 'refs/tags/v9.8.7-ransub.3'])
        self.assertTrue(publish.call_args.args[0].automatic)

    def test_automatic_publish_rejects_existing_tag_at_different_source(self):
        args = self.publication_fixture()
        with patch.object(release, 'automatic_source', return_value=('a' * 40, True)), \
                patch.object(release, 'github', return_value={'tag_name': 'v9.8.7-ransub.2'}), \
                patch.object(subprocess, 'check_output', return_value='- Accepted change\n'), \
                patch.object(subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, stdout='b' * 40)) as command, \
                patch.object(release, 'local_publish') as publish:
            with self.assertRaisesRegex(ValueError, 'different source'):
                release.automatic_publish(args)
        self.assertEqual(command.call_count, 1)
        publish.assert_not_called()

    def test_full_and_simple_matrix_match_existing_targets(self):
        full = release.targets()
        self.assertEqual(len(full), 5)
        self.assertNotIn({'goos': 'windows', 'goarch': 'arm64'}, full)
        self.assertEqual(release.targets(True), [{'goos': 'linux', 'goarch': 'amd64'}])

    def test_leaf_keeps_packaging_and_selects_only_one_target(self):
        original = release.config()
        release.generate_config(argparse.Namespace(mode='build', simple=False, goos='darwin', goarch='arm64', output='leaf.yaml'))
        leaf = yaml.safe_load(Path('leaf.yaml').read_text(encoding='utf-8'))
        self.assertEqual(leaf['builds'][0]['goos'], ['darwin'])
        self.assertEqual(leaf['builds'][0]['goarch'], ['arm64'])
        self.assertEqual(leaf['builds'][0]['ignore'], [])
        self.assertEqual(leaf['archives'], original['archives'])
        self.assertEqual(leaf['release'], original['release'])
        self.assertFalse(leaf['dockers'])
        self.assertIn('{{ .Env.RELEASE_DATE }}', '\n'.join(leaf['builds'][0]['ldflags']))

    def test_publication_config_has_no_compilation_or_docker_work(self):
        for simple in (False, True):
            with self.subTest(simple=simple):
                original = release.config(simple)
                release.generate_config(argparse.Namespace(mode='publish', simple=simple, output='publisher.yaml'))
                data = yaml.safe_load(Path('publisher.yaml').read_text(encoding='utf-8'))
                self.assertTrue(data['builds'][0]['skip'])
                self.assertFalse(data['archives'])
                self.assertFalse(data['dockers'])
                self.assertEqual(data['release']['header'], original['release']['header'])
                self.assertEqual(data['release']['footer'], original['release']['footer'])
                if simple:
                    self.assertTrue(data['checksum']['disable'])
                    self.assertTrue(data['release']['skip_upload'])
                else:
                    self.assertEqual(data['checksum']['extra_files'], data['release']['extra_files'])

    def test_collect_and_verify_hash_and_source_binding(self):
        args = self.fixture_artifacts()
        release.verify(args)
        file = next(Path(args.input).glob('*.tar.gz'))
        file.write_bytes(b'corrupted')
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            release.verify(args)

    def test_missing_extra_and_wrong_commit_artifacts_are_rejected(self):
        args = self.fixture_artifacts(True)
        args.sha = 'b' * 40
        with self.assertRaises(ValueError):
            release.verify(args)
        args.sha = 'a' * 40
        Path('release-input/unexpected').write_text('not an asset')
        with self.assertRaises(ValueError):
            release.verify(args)
        Path('release-input/unexpected').unlink()
        next(Path('release-input').glob('*.tar.gz')).unlink()
        with self.assertRaises(FileNotFoundError):
            release.verify(args)

    def test_linux_context_preserves_binary_executable_mode(self):
        args = self.fixture_artifacts()
        Path('Dockerfile.goreleaser').write_text('FROM scratch\nCOPY sub2api /sub2api\n')
        Path('deploy').mkdir()
        Path('deploy/docker-entrypoint.sh').write_text('#!/bin/sh\nexec /app/sub2api\n')
        Path('backend/resources').mkdir()
        Path('backend/resources/data').write_text('fixture')
        Path('THIRD_PARTY_NOTICES.md').write_text('BEpusdt collection engine notices\n')
        Path('LICENSES').mkdir()
        Path('LICENSES/BEpusdt-GPL-3.0.txt').write_text('BEpusdt GPL-3.0 license fixture\n')
        release.contexts(args)
        for arch in ('amd64', 'arm64'):
            binary = Path('contexts') / arch / 'sub2api'
            self.assertEqual(binary.read_bytes(), b'fixture')
            self.assertEqual(binary.stat().st_mode & 0o777, 0o755)
            context = binary.parent
            self.assertEqual((context / 'THIRD_PARTY_NOTICES.md').read_text(), Path('THIRD_PARTY_NOTICES.md').read_text())
            self.assertEqual((context / 'LICENSES/BEpusdt-GPL-3.0.txt').read_text(), Path('LICENSES/BEpusdt-GPL-3.0.txt').read_text())

    def test_plan_requires_a_tag_for_publication(self):
        args = argparse.Namespace(ref='main', dry_run=False, simple=False)
        with patch.object(subprocess, 'check_output', return_value='a' * 40 + '\n'):
            with self.assertRaisesRegex(ValueError, 'version tag'):
                release.plan(args)
        args.ref = 'v9.8.7'
        with patch.object(subprocess, 'check_output', side_effect=['a' * 40 + '\n', 'b' * 40 + '\n']):
            with self.assertRaisesRegex(ValueError, 'does not match'):
                release.plan(args)

    def test_dry_run_plan_resolves_matrix_without_a_new_tag(self):
        with patch.dict(os.environ, {'GITHUB_OUTPUT': 'outputs', 'GITHUB_REPOSITORY_OWNER': 'ExampleOwner'}), patch.object(subprocess, 'check_output', return_value='a' * 40 + '\n'):
            release.plan(argparse.Namespace(ref='feature/matrix', dry_run=True, simple=False))
        output = dict(line.split('=', 1) for line in Path('outputs').read_text(encoding='utf-8').splitlines())
        self.assertEqual(output['dry_run'], 'true')
        self.assertEqual(output['owner_lower'], 'exampleowner')
        self.assertEqual(len(json.loads(output['matrix'])['include']), 5)

    def test_docker_commands_do_not_publish_during_dry_run(self):
        fake_bin = Path('bin')
        fake_bin.mkdir()
        docker = fake_bin / 'docker'
        docker.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$DOCKER_LOG"\n')
        docker.chmod(0o755)
        env = {**os.environ, 'PATH': str(fake_bin.resolve()) + os.pathsep + os.environ['PATH'],
               'DOCKER_LOG': str(Path('docker.log').resolve()), 'RUNNER_TEMP': self.temp.name,
               'RELEASE_VERSION': '9.8.7', 'RELEASE_SHA': 'a' * 40, 'GITHUB_REPOSITORY': 'ExampleOwner/sub2api',
               'DRY_RUN': 'true', 'SIMPLE_RELEASE': 'false', 'DOCKERHUB_USERNAME': 'skip'}
        subprocess.run(['bash', str(ROOT / '.github/release-tools/release-images.sh')], env=env, check=True)
        log = Path('docker.log').read_text(encoding='utf-8')
        self.assertEqual(log.count('buildx build'), 2)
        self.assertIn('linux/arm64', log)
        self.assertNotIn('--push', log)
        self.assertNotIn('imagetools', log)
        self.assertNotIn('skip/sub2api', log)
        self.assertIn('ghcr.io/exampleowner/sub2api', log)


    def test_published_full_and_simple_image_tags(self):
        fake_bin = Path('bin')
        fake_bin.mkdir()
        docker = fake_bin / 'docker'
        docker.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$DOCKER_LOG"\n')
        docker.chmod(0o755)
        for simple in (False, True):
            with self.subTest(simple=simple):
                log_path = Path(f'docker-{simple}.log').resolve()
                env = {**os.environ, 'PATH': str(fake_bin.resolve()) + os.pathsep + os.environ['PATH'],
                       'DOCKER_LOG': str(log_path), 'RUNNER_TEMP': self.temp.name,
                       'RELEASE_VERSION': '9.8.7', 'RELEASE_SHA': 'a' * 40, 'GITHUB_REPOSITORY': 'ExampleOwner/sub2api',
                       'DRY_RUN': 'false', 'SIMPLE_RELEASE': str(simple).lower(), 'DOCKERHUB_USERNAME': 'fixturehub'}
                subprocess.run(['bash', str(ROOT / '.github/release-tools/release-images.sh')], env=env, check=True)
                log = log_path.read_text(encoding='utf-8')
                self.assertIn('--push', log)
                self.assertEqual(log.count('buildx build'), 1 if simple else 2)
                if simple:
                    self.assertNotIn('fixturehub', log)
                    self.assertNotIn('imagetools', log)
                    self.assertIn('ghcr.io/exampleowner/sub2api:latest', log)
                else:
                    self.assertEqual(log.count('imagetools create'), 2)
                    self.assertIn('fixturehub/sub2api:9.8', log)
                    self.assertIn('ghcr.io/exampleowner/sub2api:9', log)



    def test_prerelease_plan_and_image_tags_are_isolated(self):
        release.VERSION_FILE.write_text('9.8.7-rc.1\n')
        with patch.dict(os.environ, {'GITHUB_OUTPUT': 'outputs'}), patch.object(subprocess, 'check_output', return_value='a' * 40 + '\n'):
            release.plan(argparse.Namespace(ref='feature/rc', dry_run=True, simple=False))
        output = dict(line.split('=', 1) for line in Path('outputs').read_text(encoding='utf-8').splitlines())
        self.assertEqual(output['prerelease'], 'true')
        fake_bin = Path('bin')
        fake_bin.mkdir()
        docker = fake_bin / 'docker'
        docker.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$DOCKER_LOG"\n')
        docker.chmod(0o755)
        for simple in (False, True):
            with self.subTest(simple=simple):
                log_path = Path(f'rc-{simple}.log').resolve()
                env = {**os.environ, 'PATH': str(fake_bin.resolve()) + os.pathsep + os.environ['PATH'],
                       'DOCKER_LOG': str(log_path), 'RUNNER_TEMP': self.temp.name,
                       'RELEASE_VERSION': '9.8.7-rc.1', 'RELEASE_SHA': 'a' * 40, 'GITHUB_REPOSITORY': 'ExampleOwner/sub2api',
                       'DRY_RUN': 'false', 'SIMPLE_RELEASE': str(simple).lower(), 'DOCKERHUB_USERNAME': 'fixturehub'}
                subprocess.run(['bash', str(ROOT / '.github/release-tools/release-images.sh')], env=env, check=True)
                log = log_path.read_text(encoding='utf-8')
                self.assertIn('--push', log)
                self.assertIn(':9.8.7-rc.1', log)
                self.assertNotIn(':latest', log)
                self.assertNotRegex(log, r':9(?:\.8)?(?:\s|$)')
        workflow = yaml.safe_load((ROOT / '.github/workflows/release.yml').read_text(encoding='utf-8'))
        self.assertIn("prerelease != 'true'", workflow['jobs']['sync-version-file']['if'])
        notification = next(step for step in workflow['jobs']['release']['steps'] if step.get('name') == 'Send Telegram Notification')
        self.assertIn("prerelease != 'true'", notification['if'])

    def test_release_announcement_requires_explicit_opt_in(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/release.yml').read_text(encoding='utf-8'))
        trigger = workflow.get('on', workflow.get(True))
        self.assertIs(trigger['workflow_dispatch']['inputs']['notify_release']['default'], False)
        step = next(step for step in workflow['jobs']['release']['steps'] if step.get('name') == 'Send Telegram Notification')
        self.assertIn('inputs.notify_release == true', step['if'])


if __name__ == '__main__':
    unittest.main()
