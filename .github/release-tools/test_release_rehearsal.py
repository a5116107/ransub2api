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

    def test_existing_overlap_peer_is_preserved_before_creation(self):
        self.args.overlap_config = True
        run = rehearsal.Rehearsal(self.args)
        def inspect(*args, **kwargs):
            if args[:2] == ('context', 'inspect'):
                return subprocess.CompletedProcess([], 0, 'unix:///var/run/docker.sock', '')
            if args[:2] == ('image', 'inspect'):
                return subprocess.CompletedProcess([], 0, '', '')
            if args[-1] == run.prefix + '-peer':
                return subprocess.CompletedProcess([], 0, 'existing-peer', '')
            return subprocess.CompletedProcess([], 1, '', 'No such object')
        with patch.dict(os.environ, {'DOCKER_HOST': ''}), patch.object(run, 'docker', side_effect=inspect), \
                patch.object(run, 'create') as create:
            with self.assertRaisesRegex(RuntimeError, 'owner recovery.*peer'):
                run.preflight()
        create.assert_not_called()

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

    def test_api_can_address_peer_without_changing_primary(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        reply = json.dumps({'status': 200, 'data': {'code': 0, 'data': {'saved': True}}})
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, reply, '')) as docker:
            self.assertEqual(run.api('GET', '/fixture', app='d' * 64), {'saved': True})
        self.assertEqual(docker.call_args.args[:3], ('exec', '-i', 'd' * 64))
        self.assertEqual(run.app, 'c' * 64)

    def test_peer_start_preserves_primary_and_checks_its_own_binary(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        run.result['baseline_binary_sha256'] = 'old-binary-digest'
        replies = [{'status': 'ok'}, {'access_token': 'synthetic-token'}, {'required': False}]
        with patch.object(run, 'create', return_value='d' * 64) as create, \
                patch.object(run, 'api', side_effect=replies) as api, \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'old-binary-digest', '')):
            run.start_app(run.baseline, 'baseline_overlap', name='peer')
        self.assertEqual(run.app, 'c' * 64)
        self.assertIn(run.prefix + '-peer', create.call_args.args)
        self.assertTrue(all(call.kwargs['app'] == 'd' * 64 for call in api.call_args_list))
        self.assertEqual(run.result['stages'][-1]['loaded_sha256'], 'old-binary-digest')

    def test_postgres_data_uses_explicit_tmpfs_directory_across_image_versions(self):
        run = rehearsal.Rehearsal(self.args)
        with patch.object(run, 'create', return_value='c' * 64) as create, \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, '', '')), \
                patch.object(run, 'start_app', side_effect=RuntimeError('stop after backing services')):
            with self.assertRaisesRegex(RuntimeError, 'stop after backing services'):
                run.exercise()
        database = create.call_args_list[1].args
        self.assertIn('/var/lib/postgresql:rw,size=1073741824', database)
        self.assertIn('PGDATA=/var/lib/postgresql/rehearsal', database)

    def test_overlap_cleanup_stops_only_captured_peer(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        run.resources = [('container', run.app), ('container', 'd' * 64)]
        with patch.object(run, 'docker') as docker:
            run.stop_app('d' * 64)
        self.assertEqual(docker.call_args_list[0].args, ('stop', '--time', '20', 'd' * 64))
        self.assertEqual(run.resources, [('container', run.app)])
        self.assertEqual(run.app, 'c' * 64)

    def overlap_fixture(self, drop_candidate_field=False):
        run = rehearsal.Rehearsal(self.args)
        run.app, run.db, run.redis = 'c' * 64, 'e' * 64, 'f' * 64
        stored = {'enabled': False, 'preserve_session_affinity': True, 'cache_weight': 17,
                  'target_tps': 47, 'max_load_percent': 80}
        legacy_fields = set(stored) - ({'cache_weight'} if drop_candidate_field else set())
        def start(*args, **kwargs):
            run.resources.append(('container', 'd' * 64))
            run.result['stages'].append({'container_id': 'd' * 64})
            return 'synthetic-old-token'
        def api(method, path, body=None, token=None, *, app=None):
            if method == 'PUT' and 'priority-scheduling' in path:
                if drop_candidate_field and app == 'd' * 64:
                    stored.pop('cache_weight', None)
                stored.update(body)
                return dict(stored)
            if 'priority-scheduling' in path:
                return dict(stored)
            return {'status': 'ok'}
        def sql(query):
            return json.dumps(stored)
        return run, stored, legacy_fields, start, api, sql

    def test_overlap_detects_old_writer_erasing_new_fields_and_cleans_peer(self):
        run, stored, fields, start, api, sql = self.overlap_fixture(True)
        with patch.object(run, 'start_app', side_effect=start), patch.object(run, 'api', side_effect=api), \
                patch.object(run, 'sql', side_effect=sql), patch.object(run, 'snapshot', return_value='original'), \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'true', '')):
            with self.assertRaisesRegex(RuntimeError, 'baseline_save.*cache_weight'):
                run.exercise_config_overlap('/api/v1/admin/priority-scheduling/config', 1, 'original', fields)
        overlap = run.result['config_overlap']
        self.assertEqual(overlap['status'], 'incompatible')
        self.assertEqual(overlap['changed_fields'], ['cache_weight'])
        self.assertFalse(run.result['mixed_version_isolation_verified'])
        self.assertTrue(run.result['cleanup'][-1]['removed'])

    def test_overlap_reports_only_verified_control_plane_scope(self):
        run, stored, fields, start, api, sql = self.overlap_fixture()
        with patch.object(run, 'start_app', side_effect=start), patch.object(run, 'api', side_effect=api), \
                patch.object(run, 'sql', side_effect=sql), patch.object(run, 'snapshot', return_value='original'), \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'true', '')):
            run.exercise_config_overlap('/api/v1/admin/priority-scheduling/config', 1, 'original', fields)
        self.assertEqual(run.result['config_overlap']['status'], 'compatible')
        self.assertEqual(stored['cache_weight'], 29)
        self.assertEqual(stored['target_tps'], 53)
        self.assertFalse(run.result['mixed_version_isolation_verified'])
        self.assertEqual(run.result['config_overlap']['scope'], 'priority_configuration_and_account_fields')

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
