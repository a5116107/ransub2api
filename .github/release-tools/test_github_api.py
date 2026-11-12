import os
import unittest
from unittest.mock import MagicMock, patch

import github_api


class GitHubAPITest(unittest.TestCase):
    def test_only_reads_retry_after_a_lost_connection(self):
        for method, calls in (('GET', 3), ('POST', 1)):
            opener = MagicMock()
            opener.open.side_effect = github_api.RemoteDisconnected('fixture disconnect')
            with patch.dict(os.environ, {'GH_TOKEN': 'fixture-token'}), patch.object(github_api, 'build_opener', return_value=opener), patch.object(github_api.time, 'sleep'):
                with self.assertRaises(github_api.RemoteDisconnected):
                    github_api.github(method, 'repos/a5116107/ransub2api')
            self.assertEqual(opener.open.call_count, calls)

    def test_auth_uses_exact_authority_and_closes_connection(self):
        response = MagicMock()
        response.__enter__.return_value.read.return_value = b'{"ok":true}'
        opener = MagicMock()
        opener.open.return_value = response
        with patch.dict(os.environ, {'GH_TOKEN': 'fixture-token'}), patch.object(github_api, 'build_opener', return_value=opener):
            self.assertEqual(github_api.github('GET', 'repos/a5116107/ransub2api'), {'ok': True})
        request = opener.open.call_args.args[0]
        self.assertEqual(request.get_header('Authorization'), 'Bearer fixture-token')
        self.assertEqual(request.get_header('Connection'), 'close')
        self.assertEqual(opener.open.call_args.kwargs['timeout'], 30)

    def test_unexpected_authority_is_rejected_before_network(self):
        with patch.dict(os.environ, {'GH_TOKEN': 'fixture-token'}), patch.object(github_api, 'build_opener') as opener:
            with self.assertRaises(ValueError):
                github_api.github('GET', 'https://example.invalid/assets')
            opener.assert_not_called()


if __name__ == '__main__':
    unittest.main()
