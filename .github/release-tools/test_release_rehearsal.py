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

    def test_request_overlap_preserves_existing_peer_and_upstream(self):
        self.args.overlap_requests = True
        run = rehearsal.Rehearsal(self.args)
        for suffix in ('peer', 'upstream'):
            with self.subTest(suffix=suffix):
                def inspect(*args, **kwargs):
                    if args[:2] == ('context', 'inspect'):
                        return subprocess.CompletedProcess([], 0, 'unix:///var/run/docker.sock', '')
                    if args[:2] == ('image', 'inspect'):
                        return subprocess.CompletedProcess([], 0, '', '')
                    return subprocess.CompletedProcess([], 0 if args[-1] == run.prefix + '-' + suffix else 1,
                                                       '', 'No such object')
                with patch.dict(os.environ, {'DOCKER_HOST': ''}), patch.object(run, 'docker', side_effect=inspect), \
                        patch.object(run, 'create') as create:
                    with self.assertRaisesRegex(RuntimeError, 'owner recovery.*' + suffix):
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

    def test_peer_onboarding_acknowledges_peer_not_primary(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        run.result['baseline_binary_sha256'] = 'baseline-digest'
        replies = [{'status': 'ok'}, {'access_token': 'synthetic-token'},
                   {'required': True, 'ack_phrase_en': 'fixture-phrase'}, {'required': False}]
        with patch.object(run, 'create', return_value='d' * 64), \
                patch.object(run, 'api', side_effect=replies) as api, \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'baseline-digest', '')):
            run.start_app(run.baseline, 'baseline_overlap', name='peer')
        self.assertTrue(all(call.kwargs.get('app') == 'd' * 64 for call in api.call_args_list))
        self.assertEqual(run.app, 'c' * 64)

    def test_peer_startup_failure_reads_peer_logs_and_retains_identity(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        def docker(*args, **kwargs):
            if args[0] == 'inspect':
                return subprocess.CompletedProcess([], 0, 'false', '')
            return subprocess.CompletedProcess([], 0, run.password + ' peer-failure', '')
        with patch.object(run, 'create', return_value='d' * 64), \
                patch.object(run, 'api', side_effect=RuntimeError('not started')), \
                patch.object(run, 'docker', side_effect=docker) as calls:
            with self.assertRaisesRegex(RuntimeError, 'startup failed at baseline_overlap'):
                run.start_app(run.baseline, 'baseline_overlap', name='peer')
        logs = [call for call in calls.call_args_list if call.args[0] == 'logs']
        self.assertEqual(logs[0].args[-1], 'd' * 64)
        self.assertNotIn(run.password, run.result['startup_log'])
        self.assertEqual(run.app, 'c' * 64)

    def test_request_overlap_configures_same_ceiling_without_changing_default_start(self):
        self.args.overlap_requests = True
        run = rehearsal.Rehearsal(self.args)
        run.result['candidate_binary_sha256'] = 'fixture-digest'
        with patch.object(run, 'create', return_value='c' * 64) as create, \
                patch.object(run, 'api', side_effect=[{'status':'ok'}, {'access_token':'fixture'}, {'required':False}]), \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'fixture-digest', '')):
            run.start_app(Path(self.args.candidate), 'candidate')
        args = create.call_args.args
        self.assertIn('CONFIG_FILE=/app/data/rehearsal.yaml', args)
        self.assertIn('upstream_key_concurrency', args[-1])
        self.assertNotIn(rehearsal.MIXED_KEY, args[-1])
        self.assertTrue(args[-1].endswith("os.execv('/rehearsal-bin',['/rehearsal-bin'])"))
        self.assertFalse(run.result['mixed_version_isolation_verified'])

    def test_mixed_request_policy_enables_native_ws_only_inside_the_fixture(self):
        self.args.overlap_requests = True
        run = rehearsal.Rehearsal(self.args)
        run.result['candidate_binary_sha256'] = 'fixture-digest'
        with patch.object(run, 'create', return_value='c' * 64) as create, \
                patch.object(run, 'api', side_effect=[{'status':'ok'}, {'access_token':'fixture'}, {'required':False}]), \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, 'fixture-digest', '')):
            run.start_app(Path(self.args.candidate), 'candidate')
        self.assertIn('mode_router_v2_enabled', create.call_args.args[-1],
                      'the mixed fixture must exercise native WS rather than HTTP fallback')
        self.assertIn('apikey_enabled', create.call_args.args[-1])

    def test_stream_credentials_are_stdin_only_and_request_identity_is_distinct(self):
        run = rehearsal.Rehearsal(self.args)
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, '{"status":503}', '')) as docker:
            self.assertEqual(run.stream_request('c' * 64, 'synthetic-key', 'case-id'), {'status':503})
        self.assertNotIn('synthetic-key', repr(docker.call_args.args))
        self.assertEqual(json.loads(docker.call_args.kwargs['input']),
                         {'key':'synthetic-key','case':'case-id','cancel':False})

    def test_websocket_credentials_are_stdin_only_with_explicit_turn_count(self):
        run = rehearsal.Rehearsal(self.args)
        with patch.object(run, 'docker', return_value=subprocess.CompletedProcess([], 0, '{"status":101}', '')) as docker:
            self.assertEqual(run.stream_request('node', 'sensitive-key', 'ws-case', websocket=True, turns=2), {'status':101})
        self.assertNotIn('sensitive-key', repr(docker.call_args.args))
        self.assertEqual(json.loads(docker.call_args.kwargs['input']),
                         {'key':'sensitive-key','case':'ws-case','cancel':False,'websocket':True,'turns':2})

    def test_websocket_codec_bounds_and_masking(self):
        namespace = {}
        exec(rehearsal.WEBSOCKET_IO, namespace)
        for masked in (False, True):
            for size in (0, 125, 126, 65535, 65536):
                with self.subTest(masked=masked, size=size):
                    from unittest.mock import Mock
                    sock = Mock()
                    data = b'x' * size
                    namespace['ws_send'](sock, 1, data, masked)
                    self.assertEqual(namespace['ws_read'](io.BytesIO(sock.sendall.call_args.args[0]), masked), (1, data))
        for raw in (b'\x01\x00', b'\xc1\x00', b'\x81\x80', b'\x81\x7f'+(1048577).to_bytes(8, 'big'), b'\x89\x7e\x00\x7e'):
            with self.subTest(raw=raw), self.assertRaises((EOFError, ValueError)):
                namespace['ws_read'](io.BytesIO(raw), False)
        self.assertEqual(namespace['ws_accept']('dGhlIHNhbXBsZSBub25jZQ=='), 's3pPLMBiTxaQ9kYGzzhZRbK+xOo=')

    def test_native_websocket_fixture_roundtrip_and_continuation_are_real(self):
        import socket
        import threading
        namespace = {}
        code = rehearsal.REQUEST_UPSTREAM.replace("ThreadingHTTPServer(('0.0.0.0',8787),Handler).serve_forever()",
                                                   "server=ThreadingHTTPServer(('127.0.0.1',0),Handler)")
        exec(code, namespace)
        server = namespace['server']
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        connect = socket.create_connection
        try:
            for turns in (1, 2):
                stdout = io.StringIO()
                payload = {'key':'private-fixture-key','case':'fixture-ws','websocket':True,'turns':turns}
                with patch('sys.stdin', io.StringIO(json.dumps(payload))), patch('sys.stdout', stdout), \
                        patch('socket.create_connection', side_effect=lambda addr, **kwargs: connect(server.server_address, **kwargs)):
                    with self.assertRaises(SystemExit) as done:
                        exec(rehearsal.STREAM_CLIENT, {})
                self.assertEqual(done.exception.code, 0)
                result = json.loads(stdout.getvalue())
                self.assertEqual(result['status'], 101)
                self.assertEqual(result['completed'], turns)
                self.assertEqual(len(set(result['response_ids'])), turns)
                self.assertEqual(result['errors'], 0)
                self.assertEqual(result['close_code'], 0)
                self.assertNotIn('private-fixture-key', stdout.getvalue())
            self.assertEqual(namespace['state']['ws_calls'], 3)
            self.assertEqual(namespace['state']['ws_continuations'], 1)
            self.assertEqual(namespace['state']['peak'], 1)
            self.assertEqual(namespace['state']['active'], 0)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
        self.assertFalse(thread.is_alive())

    def test_native_websocket_client_cancels_before_accepting_terminal(self):
        import socket
        import threading
        import time
        namespace = {}
        code = rehearsal.REQUEST_UPSTREAM.replace("ThreadingHTTPServer(('0.0.0.0',8787),Handler).serve_forever()",
                                                   "server=ThreadingHTTPServer(('127.0.0.1',0),Handler)")
        exec(code, namespace)
        server = namespace['server']
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        connect = socket.create_connection
        try:
            for cancel in (True, 'abort'):
                with self.subTest(cancel=cancel):
                    with namespace['lock']:
                        namespace['state']['hold_next']=True
                        namespace['state']['partial_usage_next']=True
                        namespace['gate'].clear()
                    stdout = io.StringIO()
                    payload = {'key':'private-fixture-key','case':'cancel-fixture','websocket':True,'cancel':cancel}
                    with patch('sys.stdin', io.StringIO(json.dumps(payload))), patch('sys.stdout', stdout), \
                            patch('socket.create_connection', side_effect=lambda addr, **kwargs: connect(server.server_address, **kwargs)):
                        with self.assertRaises(SystemExit) as done:
                            exec(rehearsal.STREAM_CLIENT, {})
                    self.assertEqual(done.exception.code, 0)
                    result = json.loads(stdout.getvalue())
                    self.assertTrue(result['cancelled'], 'WebSocket cancellation must stop before accepting a completed terminal')
                    self.assertEqual(result['completed'], 0)
                    self.assertGreater(result['deltas'], 0)
                    self.assertTrue(result['active_response_id'].startswith('resp_rehearsal_'))
                    self.assertEqual(result['response_ids'], [])
                    self.assertNotIn('private-fixture-key', stdout.getvalue())
                    deadline = time.monotonic() + 3
                    while namespace['state']['active']:
                        self.assertLess(time.monotonic(), deadline, 'physical upstream stayed active after cancellation')
                        time.sleep(.01)
                    self.assertEqual(namespace['state']['ws_cancelled'], 1 if cancel is True else 2)
        finally:
            namespace['gate'].set()
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
        self.assertFalse(thread.is_alive())

    def test_websocket_settlement_uses_upstream_turn_id_not_connection_id(self):
        run = rehearsal.Rehearsal(self.args)
        observed = {'usage':1,'claims':1,'quotes':1,'account':3,'group':4,'rate':.12,'input_tokens':16,'output_tokens':4,
                    'canonical_matches':True,'debit_matches':True}
        with patch.object(run, 'sql', return_value=json.dumps(observed)) as sql:
            run.verify_request_settlement('resp_fixture_1', 3, 4, .12, websocket=True)
        self.assertIn("request_id='resp_fixture_1'", sql.call_args.args[0])
        self.assertNotIn('client:resp_fixture_1', sql.call_args.args[0])

    def test_websocket_overlap_failed_state_never_claims_compatibility(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'candidate'
        with patch.object(run, 'sql', return_value='{}'), patch.object(run, 'wait_request_state', side_effect=RuntimeError('fixture unavailable')):
            with self.assertRaisesRegex(RuntimeError, 'fixture unavailable'):
                run.exercise_websocket_overlap('peer', [], [], [], [])
        self.assertEqual(run.result['websocket_overlap']['status'], 'failed')
        self.assertFalse(run.result['mixed_version_isolation_verified'])

    def test_cancelled_websocket_settlement_keeps_only_observed_turn_usage(self):
        run = rehearsal.Rehearsal(self.args)
        observed = {'usage':1,'claims':1,'quotes':1,'account':3,'group':4,'rate':.12,'input_tokens':3,'output_tokens':2,
                    'canonical_matches':True,'debit_matches':True}
        with patch.object(run, 'sql', return_value=json.dumps(observed)):
            self.assertEqual(run.verify_request_settlement('resp_partial', 3, 4, .12, websocket=True,
                                                           input_tokens=3, output_tokens=2), observed)
            with self.assertRaisesRegex(RuntimeError, 'actual current upstream turn'):
                run.verify_request_settlement('resp_partial', 3, 4, .12, websocket=True)

    def test_stream_client_observes_terminal_and_cancellation_without_echoing_credentials(self):
        from unittest.mock import MagicMock
        events = [b'data: {"type":"response.output_text.delta","delta":"fixture OK"}\n',
                  b'data: {"type":"response.completed"}\n']
        for cancel in (False, True):
            with self.subTest(cancel=cancel):
                response = MagicMock()
                response.__enter__.return_value = response
                response.status, response.headers = 200, {'x-client-request-id':'fixture-id'}
                response.__iter__.return_value = iter(events)
                stdout = io.StringIO()
                with patch('sys.stdin', io.StringIO(json.dumps({'key':'synthetic-sensitive-key','case':'fixture','cancel':cancel}))), \
                        patch('sys.stdout', stdout), patch('urllib.request.build_opener') as opener:
                    opener.return_value.open.return_value = response
                    exec(rehearsal.STREAM_CLIENT, {})
                observed = json.loads(stdout.getvalue())
                self.assertEqual(observed['cancelled'], cancel)
                self.assertEqual(observed['completed'], 0 if cancel else 1)
                self.assertEqual(observed['deltas'], 1)
                self.assertNotIn('synthetic-sensitive-key', stdout.getvalue())
                self.assertEqual(opener.return_value.open.call_args.kwargs['timeout'], 15)
                response.__exit__.assert_called_once()

    def test_stream_client_retains_typed_admission_error_not_private_body(self):
        from urllib.error import HTTPError
        body = json.dumps({'error':{'code':'upstream_key_admission_unavailable','message':'sensitive-body'}}).encode()
        error = HTTPError('http://127.0.0.1:8080/v1/responses',503,'unavailable',
                          {'x-client-request-id':'fixture-id'},io.BytesIO(body))
        stdout = io.StringIO()
        with patch('sys.stdin', io.StringIO(json.dumps({'key':'synthetic-key','case':'fixture'}))), \
                patch('sys.stdout', stdout), patch('urllib.request.build_opener') as opener:
            opener.return_value.open.side_effect = error
            exec(rehearsal.STREAM_CLIENT, {})
        observed = json.loads(stdout.getvalue())
        self.assertEqual(observed['status'], 503)
        self.assertEqual(observed['error_code'], 'upstream_key_admission_unavailable')
        self.assertEqual(observed['client_request_id'], 'fixture-id')
        self.assertNotIn('sensitive-body', stdout.getvalue())

    def test_missing_slot_observation_fails_with_bounded_nonsecret_state(self):
        run = rehearsal.Rehearsal(self.args)
        run.redis = 'redis'
        with patch.object(run, 'fixture_api', return_value={'active':0,'calls':0}), \
                patch.object(run, 'docker', return_value=subprocess.CompletedProcess([],0,'0','')), \
                patch.object(rehearsal.time, 'monotonic', side_effect=[0,16]):
            with self.assertRaisesRegex(RuntimeError, 'state did not converge.*expected_slots'):
                run.wait_request_state(active=1,slots=1,calls=1)

    def test_settlement_rejects_bad_stream_before_sql(self):
        run = rehearsal.Rehearsal(self.args)
        good = {'status':200,'completed':1,'deltas':1,'errors':0,'cancelled':False,'client_request_id':'valid-id'}
        for field, value in [('status',503),('completed',0),('completed',2),('deltas',0),('errors',1),('cancelled',True),
                             ('client_request_id',"invalid' SQL")]:
            with self.subTest(field=field,value=value), patch.object(run, 'sql') as sql:
                with self.assertRaises(RuntimeError):
                    run.verify_stream_settlement(dict(good, **{field:value}), 3, 4, .12)
                sql.assert_not_called()

    def test_settlement_checks_unique_rows_price_and_debit_owner(self):
        run = rehearsal.Rehearsal(self.args)
        response = {'status':200,'completed':1,'deltas':1,'errors':0,'cancelled':False,'client_request_id':'valid-id'}
        observed = {'usage':1,'claims':1,'quotes':1,'account':3,'group':4,'rate':.12,'input_tokens':16,'output_tokens':4,
                    'canonical_matches':True,'debit_matches':True}
        with patch.object(run, 'sql', return_value=json.dumps(observed)):
            self.assertEqual(run.verify_stream_settlement(response, 3, 4, .12), observed)
        for field,value in [('usage',2),('claims',2),('quotes',2),('account',9),('group',9),('rate',.15),
                            ('canonical_matches',False),('debit_matches',False),('input_tokens',3),('output_tokens',2)]:
            with self.subTest(field=field), patch.object(run, 'sql', return_value=json.dumps(dict(observed, **{field:value}))):
                with self.assertRaises(RuntimeError):
                    run.verify_stream_settlement(response, 3, 4, .12)

    def test_request_overlap_failure_never_claims_full_mixed_compatibility_and_stops_peer(self):
        run = rehearsal.Rehearsal(self.args)
        run.app = 'c' * 64
        run.db, run.redis, run.upstream = 'db', 'redis', 'upstream'
        def start(*args, **kwargs):
            run.result['stages'].append({'container_id':'d' * 64})
        with patch.object(run, 'start_app', side_effect=start), patch.object(run, 'docker',
                return_value=subprocess.CompletedProcess([], 0, 'false', '')), patch.object(run, 'stop_app') as stop:
            with self.assertRaisesRegex(RuntimeError, 'both mixed-request instances'):
                run.exercise_request_overlap('synthetic-token')
        stop.assert_called_once_with('d' * 64)
        self.assertEqual(run.result['request_overlap']['status'], 'failed')
        self.assertFalse(run.result['mixed_version_isolation_verified'])

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
