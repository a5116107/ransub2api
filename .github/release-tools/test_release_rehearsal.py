import argparse
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import release_rehearsal as rehearsal


class ReleaseRehearsalTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.args = argparse.Namespace(candidate_sha='a' * 40, candidate=str(root / 'candidate'),
            baseline=str(root / 'release'), scratch=str(root / 'scratch'), output=str(root / 'result.json'),
            runtime_image='fixture-runtime', postgres_image='fixture-postgres', redis_image='fixture-redis')
        header = bytearray(20)
        header[:6], header[18:20] = b'\x7fELF\x02\x01', b'\x3e\x00'
        Path(self.args.candidate).write_bytes(header)
        baseline = Path(self.args.baseline)
        (baseline / 'input').mkdir(parents=True)
        (baseline / 'release-manifest.json').write_text(json.dumps({
            'repository': 'a5116107/ransub2api', 'sha': 'b' * 40, 'version': '2.19.2-ransub.2', 'simple': True}))
        with tarfile.open(baseline / 'input/sub2api_2.19.2-ransub.2_linux_amd64.tar.gz', 'w:gz') as archive:
            info = tarfile.TarInfo('sub2api')
            info.size = len(header)
            archive.addfile(info, io.BytesIO(header))

    def invoke(self):
        rehearsal.local_rehearse(self.args, lambda args: None, lambda path: 'fixture-digest')

    def test_existing_scratch_is_preserved_on_preflight_failure(self):
        root = Path(self.args.scratch)
        root.mkdir()
        baseline = root / 'baseline-sub2api'
        baseline.write_bytes(b'previous unfinished rehearsal')
        with patch.object(rehearsal.Rehearsal, 'preflight', side_effect=RuntimeError('existing scratch')):
            with self.assertRaisesRegex(RuntimeError, 'local rehearsal failed'):
                self.invoke()
        self.assertEqual(baseline.read_bytes(), b'previous unfinished rehearsal')

    def test_bad_architecture_stops_before_docker(self):
        Path(self.args.candidate).write_bytes(b'not an ELF')
        run = rehearsal.Rehearsal(self.args)
        with patch.object(run, 'docker') as docker:
            with self.assertRaisesRegex(RuntimeError, 'Linux amd64 ELF'):
                run.preflight()
        docker.assert_not_called()

    def test_remote_daemon_stops_before_resource_creation(self):
        run = rehearsal.Rehearsal(self.args)
        with patch.dict(os.environ, {'DOCKER_HOST': ''}), patch.object(run, 'docker',
                return_value=subprocess.CompletedProcess([], 0, 'ssh://remote', '')) as docker:
            with self.assertRaisesRegex(RuntimeError, 'local Docker'):
                run.preflight()
        self.assertEqual(docker.call_count, 1)

    def test_remote_host_override_stops_before_docker(self):
        run = rehearsal.Rehearsal(self.args)
        with patch.dict(os.environ, {'DOCKER_HOST': 'tcp://remote:2376'}), patch.object(run, 'docker') as docker:
            with self.assertRaisesRegex(RuntimeError, 'local Docker'):
                run.preflight()
        docker.assert_not_called()

    def test_cleanup_preserves_resource_with_different_owner(self):
        run = rehearsal.Rehearsal(self.args)
        run.resources = [('container', 'c' * 64)]
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'other-owner', '')) as docker:
            run.cleanup()
        self.assertEqual(docker.call_count, 1)
        self.assertFalse(run.result['cleanup'][0]['removed'])

    def test_cleanup_engine_failure_is_not_claimed_as_removed(self):
        run = rehearsal.Rehearsal(self.args)
        run.resources = [('container', 'c' * 64)]
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 1, '', 'engine disconnected')):
            run.cleanup()
        self.assertFalse(run.result['cleanup'][0]['removed'])

    def test_api_uses_container_loopback_with_credentials_only_on_stdin(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        reply = json.dumps({'status': 200, 'data': {'code': 0, 'data': {'saved': True}}})
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, reply, '')) as docker:
            self.assertEqual(run.api('PUT', '/fixture', {'value': 47}, 'synthetic-token'), {'saved': True})
        call = docker.call_args
        self.assertNotIn('synthetic-token', repr(call.args))
        payload = json.loads(call.kwargs['input'])
        self.assertEqual(payload['headers']['Authorization'], 'Bearer synthetic-token')
        self.assertIn('http://127.0.0.1:8080', call.args[-1])

    def test_api_errors_do_not_expose_response_or_credentials(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        reply = json.dumps({'status': 403, 'data': {'secret': 'synthetic-token'}})
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, reply, '')):
            with self.assertRaisesRegex(RuntimeError, 'HTTP 403') as error:
                run.api('GET', '/fixture', token='synthetic-token')
        self.assertNotIn('synthetic-token', str(error.exception))

    def test_fresh_fixture_completes_existing_onboarding_before_admin_calls(self):
        run = rehearsal.Rehearsal(self.args)
        run.result['candidate_binary_sha256'] = 'fixture-digest'
        replies = [{'status': 'ok'}, {'access_token': 'synthetic-token'},
                   {'required': True, 'ack_phrase_en': 'fixture-phrase'}, {'required': False}]
        with patch.object(run, 'create', return_value='c' * 64), patch.object(run, 'api', side_effect=replies) as api, \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'fixture-digest', '')):
            self.assertEqual(run.start_app(Path(self.args.candidate), 'baseline'), 'synthetic-token')
        self.assertEqual(api.call_args_list[-1].args, ('POST', '/api/v1/admin/compliance/accept',
                         {'language': 'en', 'phrase': 'fixture-phrase'}, 'synthetic-token'))
        self.assertTrue(run.result['stages'][0]['fixture_onboarding'])

    def test_wrong_running_binary_stops_before_admin_mutations(self):
        run = rehearsal.Rehearsal(self.args)
        run.result['candidate_binary_sha256'] = 'expected-digest'
        with patch.object(run, 'create', return_value='c' * 64), patch.object(run, 'api', return_value={'status': 'ok'}) as api, \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'wrong-digest', '')):
            with self.assertRaisesRegex(RuntimeError, 'running binary identity mismatch'):
                run.start_app(Path(self.args.candidate), 'candidate')
        self.assertEqual(api.call_count, 1)
        self.assertEqual(run.result['stages'], [])

    def test_docker_timeout_does_not_echo_command_credentials(self):
        run = rehearsal.Rehearsal(self.args)
        with patch.object(subprocess, 'run', side_effect=subprocess.TimeoutExpired(['docker', run.password], 180)):
            with self.assertRaisesRegex(RuntimeError, 'Docker operation timed out') as error:
                run.docker('run', '--env', 'SECRET=' + run.password)
        self.assertNotIn(run.password, str(error.exception))

    def test_cleanup_uses_captured_ids_in_reverse_dependency_order(self):
        run = rehearsal.Rehearsal(self.args)
        run.resources = [('network', 'b' * 64), ('container', 'c' * 64)]
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, run.owner, '')) as docker:
            run.cleanup()
        removals = [call.args for call in docker.call_args_list if call.args[0] in ('rm', 'network')]
        self.assertEqual(removals, [('rm', '-f', '-v', 'c' * 64), ('network', 'inspect', '--format',
            '{{index .Labels "io.ransub2api.rehearsal"}}', 'b' * 64), ('network', 'rm', 'b' * 64)])
        self.assertTrue(all(item['removed'] for item in run.result['cleanup']))

    def test_candidate_identity_mismatch_cleans_only_extracted_binary(self):
        with patch.object(rehearsal.Rehearsal, 'preflight'), patch.object(rehearsal.Rehearsal, 'version',
                return_value='commit: ' + 'b' * 40), patch.object(rehearsal.Rehearsal, 'exercise') as exercise:
            with self.assertRaisesRegex(RuntimeError, 'local rehearsal failed'):
                self.invoke()
        exercise.assert_not_called()
        self.assertTrue(Path(self.args.candidate).exists())
        self.assertFalse((Path(self.args.scratch) / 'baseline-sub2api').exists())
        result = json.loads(Path(self.args.output).read_text())
        self.assertEqual(result['error'], 'candidate binary source mismatch')
        self.assertFalse(result['passed'])


if __name__ == '__main__':
    unittest.main()
