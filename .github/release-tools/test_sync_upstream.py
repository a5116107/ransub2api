import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('sync_upstream', Path(__file__).with_name('sync_upstream.py'))
sync = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sync)


class SyncUpstreamTest(unittest.TestCase):
    def test_actions_checkout_remote_without_git_suffix_is_accepted(self):
        responses = [
            {'full_name': sync.REPOSITORY, 'default_branch': sync.BASE},
            {'tag_name': 'v2.21.0', 'draft': False, 'prerelease': False}
        ]
        results = [subprocess.CompletedProcess([], 0, 'https://github.com/a5116107/ransub2api\n'),
                   subprocess.CompletedProcess([], 0, ''), subprocess.CompletedProcess([], 0, 'a' * 40),
                   subprocess.CompletedProcess([], 0, ''), subprocess.CompletedProcess([], 0, 'b' * 40)]
        with patch.object(sync, 'gh_json', side_effect=responses), patch.object(sync, 'git', side_effect=results), patch.object(sync, 'integration_status', return_value='conflicts'):
            result = sync.sync(dry_run=True)
        self.assertEqual(result['status'], 'conflicts')
        self.assertTrue(result['dry_run'])

    def test_real_merge_preview_retains_custom_head_and_files(self):
        with tempfile.TemporaryDirectory() as directory:
            previous = Path.cwd()
            try:
                os.chdir(directory)
                sync.git('init', '-b', 'production')
                sync.git('config', 'user.name', 'Release Test Fixture')
                sync.git('config', 'user.email', 'fixture@example.invalid')
                Path('shared').write_text('base\n')
                sync.git('add', 'shared')
                sync.git('commit', '-m', 'base')
                sync.git('checkout', '-b', 'upstream')
                Path('shared').write_text('upstream\n')
                sync.git('commit', '-am', 'upstream change')
                source = sync.git('rev-parse', 'HEAD').stdout.strip()
                sync.git('checkout', 'production')
                Path('custom').write_text('retained custom feature\n')
                sync.git('add', 'custom')
                sync.git('commit', '-m', 'custom feature')
                base = sync.git('rev-parse', 'HEAD').stdout.strip()
                self.assertEqual(sync.integration_status(base, source), 'ready_for_review')
                self.assertEqual(sync.git('rev-parse', 'HEAD').stdout.strip(), base)
                self.assertEqual(Path('shared').read_text(), 'base\n')
                Path('shared').write_text('custom behavior\n')
                sync.git('commit', '-am', 'custom behavior')
                base = sync.git('rev-parse', 'HEAD').stdout.strip()
                self.assertEqual(sync.integration_status(base, source), 'conflicts')
                self.assertEqual(Path('custom').read_text(), 'retained custom feature\n')
                self.assertEqual(Path('shared').read_text(), 'custom behavior\n')
                self.assertEqual(sync.git('status', '--porcelain').stdout, '')
            finally:
                os.chdir(previous)

    def test_local_sync_does_not_push_upstream_code_with_user_credentials(self):
        responses = [
            {'full_name': sync.REPOSITORY, 'default_branch': sync.BASE},
            {'tag_name': 'v2.21.0', 'draft': False, 'prerelease': False}
        ]
        results = [subprocess.CompletedProcess([], 0, 'https://github.com/a5116107/ransub2api\n'),
                   subprocess.CompletedProcess([], 0, ''), subprocess.CompletedProcess([], 0, 'a' * 40),
                   subprocess.CompletedProcess([], 0, ''), subprocess.CompletedProcess([], 0, 'b' * 40)]
        with patch.dict(os.environ, {}, clear=True), patch.object(sync, 'gh_json', side_effect=responses), patch.object(sync, 'git', side_effect=results) as git, patch.object(sync, 'integration_status', return_value='conflicts'):
            with self.assertRaisesRegex(ValueError, 'workflow_dispatch'):
                sync.sync()
        self.assertFalse(any(call.args[0] == 'push' for call in git.call_args_list))

    def test_integrated_release_does_not_preview_or_publish(self):
        with patch.object(sync, 'git', return_value=subprocess.CompletedProcess([], 0, '')) as git:
            self.assertEqual(sync.integration_status('custom', 'source'), 'integrated')
            self.assertEqual(git.call_count, 1)

    def test_clean_merge_and_conflicts_are_both_review_only(self):
        for code, status in ((0, 'ready_for_review'), (1, 'conflicts')):
            with self.subTest(status=status), patch.object(sync, 'git', side_effect=[
                subprocess.CompletedProcess([], 1, ''), subprocess.CompletedProcess([], code, '')]):
                self.assertEqual(sync.integration_status('custom', 'source'), status)

    def test_preview_error_is_not_confused_with_merge_conflicts(self):
        with patch.object(sync, 'git', side_effect=[
            subprocess.CompletedProcess([], 1, ''), subprocess.CompletedProcess([], 128, '')]):
            with self.assertRaises(RuntimeError):
                sync.integration_status('custom', 'source')

    def test_rejects_wrong_default_branch_before_mutation(self):
        with patch.object(sync, 'gh_json', return_value={'full_name': sync.REPOSITORY, 'default_branch': 'wrong'}), patch.object(sync, 'git') as git:
            with self.assertRaises(ValueError):
                sync.sync()
            git.assert_not_called()

    def test_rejects_wrong_push_remote(self):
        with patch.object(sync, 'gh_json', return_value={'full_name': sync.REPOSITORY, 'default_branch': sync.BASE}), patch.object(sync, 'git', return_value=subprocess.CompletedProcess([], 0, 'https://github.com/ranxi2001/sub2api.git')) as git:
            with self.assertRaises(ValueError):
                sync.sync()
            self.assertEqual(git.call_count, 1)

    def test_workflow_has_no_build_release_or_merge_step(self):
        text = (Path(__file__).parents[1] / 'workflows/sync-upstream.yml').read_text(encoding='utf-8')
        self.assertIn('23 */6 * * *', text)
        self.assertIn('ref: production', text)
        self.assertIn('token: ${{ github.token }}', text)
        self.assertIn('GH_TOKEN: ${{ github.token }}', text)
        self.assertNotIn('pull_request_target', text)
        for executable in ('go test', 'pnpm', 'git merge ', 'gh pr merge', 'goreleaser'):
            self.assertNotIn(executable, text)


if __name__ == '__main__':
    unittest.main()
