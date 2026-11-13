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

    def test_api_can_assert_revocation_without_exposing_an_error_body(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        reply = json.dumps({'status': 401, 'data': {'secret': 'synthetic-token'}})
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, reply, '')):
            self.assertIsNone(run.api('GET', '/v1/usage', expected_status=401))
        reply = json.dumps({'status': 200, 'data': {}})
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, reply, '')):
            with self.assertRaisesRegex(RuntimeError, 'HTTP 200'):
                run.api('GET', '/v1/usage', expected_status=401)

    def test_api_targets_original_node_without_changing_current_selection(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        reply = json.dumps({'status': 200, 'data': {'code': 0, 'data': {'saved': True}}})
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, reply, '')) as docker:
            self.assertEqual(run.api('GET', '/fixture', container='d' * 64), {'saved': True})
        self.assertEqual(docker.call_args.args[2], 'd' * 64)
        self.assertEqual(run.app, 'c' * 64)

    def test_overlapping_node_uses_an_independent_container_name(self):
        run = rehearsal.Rehearsal(self.args)
        run.result['candidate_binary_sha256'] = 'fixture-digest'
        replies = [{'status': 'ok'}, {'access_token': 'synthetic-token'}, {'required': False}]
        with patch.object(run, 'create', return_value='c' * 64) as create, \
                patch.object(run, 'api', side_effect=replies), \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'fixture-digest', '')):
            run.start_app(Path(self.args.candidate), 'mixed', slot='legacy')
        self.assertIn(run.prefix + '-legacy', create.call_args.args)

    def test_l1_fixture_settings_are_explicit_and_default_to_disabled(self):
        for enabled in (False, True):
            with self.subTest(l1_enabled=enabled):
                run = rehearsal.Rehearsal(self.args)
                run.result['candidate_binary_sha256'] = 'fixture-digest'
                replies = [{'status': 'ok'}, {'access_token': 'synthetic-token'}, {'required': False}]
                with patch.object(run, 'create', return_value='c' * 64) as create, \
                        patch.object(run, 'api', side_effect=replies), \
                        patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'fixture-digest', '')):
                    run.start_app(Path(self.args.candidate), 'l1-fixture', l1_enabled=enabled)
                self.assertIn('API_KEY_AUTH_CACHE_L1_SIZE=' + ('1024' if enabled else '0'), create.call_args.args)
                self.assertIn('API_KEY_AUTH_CACHE_L1_TTL_SECONDS=600', create.call_args.args)

    def test_invalid_node_slot_stops_before_creating_anything(self):
        run = rehearsal.Rehearsal(self.args)
        with patch.object(run, 'create') as create:
            with self.assertRaisesRegex(RuntimeError, 'invalid fixture application slot'):
                run.start_app(Path(self.args.candidate), 'mixed', slot='unowned')
        create.assert_not_called()

    def test_mixed_state_uses_a_grouped_key_and_checks_both_snapshot_versions(self):
        scenarios = ((None, None), ('stale-context', 'candidate accepted a stale authentication policy'),
                     ('lost-limit', 'authentication lost the shared key limit'),
                     ('unstable-version', 'auth snapshot version is unstable'),
                     ('billable-usage', 'read-only probes generated billable usage'),
                     ('lost-key-save', 'mixed legacy key save was lost'),
                     ('lost-account-save', 'mixed legacy account save was lost'))
        for failure, message in scenarios:
            with self.subTest(failure=failure):
                run = rehearsal.Rehearsal(self.args)
                run.app, run.db, run.redis = 'candidate-node', 'shared-db', 'shared-redis'
                ceiling = 0
                cache_reads = 0
                key_name = None

                def start(*args, **kwargs):
                    run.app = 'legacy-node'
                    return 'legacy-token'

                def api(method, path, body=None, token=None, *, container=None):
                    nonlocal ceiling, key_name
                    if (method, path) == ('POST', '/api/v1/admin/groups'):
                        self.assertEqual(container, 'candidate-node')
                        self.assertEqual(body['platform'], 'openai')
                        self.assertFalse(body['is_exclusive'])
                        self.assertEqual(body['subscription_type'], 'standard')
                        return {'id': 42}
                    if (method, path) == ('POST', '/api/v1/keys'):
                        self.assertEqual(body['group_id'], 42)
                        return {'id': 3, 'key': 'synthetic-key'}
                    if path == '/api/v1/admin/api-keys/3':
                        ceiling = body['context_token_limit']
                    if method == 'PUT' and path == '/api/v1/keys/3':
                        key_name = body['name']
                    if method == 'GET' and path == '/api/v1/keys/3':
                        return {'name': 'unchanged' if failure == 'lost-key-save' else key_name,
                                'context_token_limit': ceiling, 'allow_long_context': False, 'concurrency_limit': 3}
                    if path == '/api/v1/admin/accounts/1':
                        return {'name': 'unchanged' if failure == 'lost-account-save' else body['name']}
                    return {}

                def docker(*args, **kwargs):
                    nonlocal cache_reads
                    if args[0] == 'inspect':
                        output = 'true\ntrue\n'
                    else:
                        version = 1 if cache_reads % 2 == 0 else 2
                        cache_reads += 1
                        if failure == 'unstable-version' and cache_reads == 3:
                            version = 9
                        snapshot = {'version': version,
                                    'context_token_limit': 0 if failure == 'stale-context' else ceiling,
                                    'allow_long_context': False,
                                    'concurrency_limit': 4 if failure == 'lost-limit' else 3}
                        output = json.dumps({'snapshot': snapshot})
                    return subprocess.CompletedProcess([], 0, output, '')

                def sql(query):
                    if failure == 'billable-usage' and 'usage_logs' in query:
                        return '1'
                    return '2' if 'pg_stat_activity' in query else '0'

                with patch.object(run, 'start_app', side_effect=start), \
                        patch.object(run, 'api', side_effect=api) as calls, \
                        patch.object(run, 'docker', side_effect=docker), \
                        patch.object(run, 'sql', side_effect=sql), \
                        patch.object(run, 'migrations', return_value={}), \
                        patch.object(run, 'snapshot', return_value='original'):
                    if failure is None:
                        run.exercise_shared_state(1, 'original', 'candidate-token')
                    else:
                        with self.assertRaisesRegex(RuntimeError, message):
                            run.exercise_shared_state(1, 'original', 'candidate-token')
                probe = run.result['mixed_shared_state']
                if failure is not None:
                    self.assertFalse(probe['passed'])
                    continue
                self.assertFalse(run.result['mixed_version_isolation_verified'])
                self.assertEqual(probe['generation_requests'], 0)
                self.assertTrue(probe['passed'])
                self.assertEqual(len(probe['rounds']), 3)
                self.assertEqual(cache_reads, 12)
                self.assertTrue(all(row['old_snapshot_reloaded'] for row in probe['rounds']))
                usage_nodes = [call.kwargs['container'] for call in calls.call_args_list if call.args[1] == '/v1/usage']
                self.assertEqual(usage_nodes, ['legacy-node', 'candidate-node'] * 6)

    def test_mixed_state_stops_before_creating_a_key_if_group_is_invalid(self):
        for invalid_id in (None, 0, -1, True, '42'):
            with self.subTest(group_id=invalid_id):
                run = rehearsal.Rehearsal(self.args)
                run.app, run.db, run.redis = 'candidate-node', 'shared-db', 'shared-redis'

                def start(*args, **kwargs):
                    run.app = 'legacy-node'
                    return 'legacy-token'

                with patch.object(run, 'start_app', side_effect=start), \
                        patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'true\ntrue\n', '')), \
                        patch.object(run, 'sql', return_value='2'), \
                        patch.object(run, 'migrations', return_value={}), \
                        patch.object(run, 'api', return_value={'id': invalid_id}) as api:
                    with self.assertRaisesRegex(RuntimeError, 'fixture group creation'):
                        run.exercise_shared_state(1, 'original', 'candidate-token')
                self.assertEqual(api.call_count, 1)
                self.assertEqual(api.call_args.args[1], '/api/v1/admin/groups')

    def test_preflight_preserves_an_existing_legacy_node(self):
        run = rehearsal.Rehearsal(self.args)

        def inspect(*args, **kwargs):
            if args[:2] == ('image', 'inspect') or args[-1] == run.prefix + '-legacy':
                return subprocess.CompletedProcess([], 0, 'existing', '')
            return subprocess.CompletedProcess([], 1, '', 'No such object')

        with patch.dict(os.environ, {'DOCKER_HOST': 'npipe://fixture'}), \
                patch.object(run, 'docker', side_effect=inspect), patch.object(run, 'create') as create:
            with self.assertRaisesRegex(RuntimeError, 'existing rehearsal resource requires owner recovery'):
                run.preflight()
        create.assert_not_called()
        self.assertEqual(run.resources, [])

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
