"""Exercise a drained binary upgrade and rollback using disposable local data."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
import re
import secrets
import subprocess
import tarfile
import time
from pathlib import Path


LABEL = 'io.ransub2api.rehearsal'
HTTP_CLIENT = '''import json,sys
from urllib.request import Request,ProxyHandler,build_opener
from urllib.error import HTTPError,URLError
p=json.load(sys.stdin)
r=Request('http://127.0.0.1:8080'+p['path'],headers=p['headers'],method=p['method'],
          data=None if p['body'] is None else json.dumps(p['body']).encode())
try:
    with build_opener(ProxyHandler({})).open(r,timeout=4) as response:
        result={'status':response.status,'data':json.load(response)}
except HTTPError as error:
    result={'status':error.code}
except (URLError,TimeoutError,ConnectionError):
    result={'status':0}
print(json.dumps(result))
'''


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class Rehearsal:
    def __init__(self, args):
        self.args = args
        self.owner = args.candidate_sha
        self.prefix = 'ransub-upgrade-' + self.owner[:12]
        self.resources = []
        self.password = secrets.token_urlsafe(32) + '!Aa9'
        self.jwt = secrets.token_hex(32)
        self.baseline = Path(args.scratch).resolve() / 'baseline-sub2api'
        self.baseline_owned = False
        self.result = {'source': self.owner, 'production_changed': False,
                       'rollout_mode': 'drained_serial', 'mixed_version_isolation_verified': False,
                       'stages': [], 'cleanup': [], 'passed': False}

    def docker(self, *args, input=None, check=True):
        try:
            result = subprocess.run(['docker', *args], input=input, capture_output=True,
                                    text=True, encoding='utf-8', timeout=180)
        except subprocess.TimeoutExpired:
            raise RuntimeError('Docker operation timed out: ' + args[0]) from None
        if check and result.returncode:
            # Commands may carry generated credentials. Do not include argv or
            # unfiltered application/engine output in errors or evidence.
            raise RuntimeError('Docker operation failed: ' + args[0])
        return result

    def preflight(self):
        require(re.fullmatch(r'[0-9a-f]{40}', self.owner), 'invalid candidate source SHA')
        candidate = Path(self.args.candidate).resolve()
        require(candidate.is_file() and not candidate.is_symlink(), 'candidate must be a regular file')
        with candidate.open('rb') as stream:
            header = stream.read(20)
        require(header[:6] == b'\x7fELF\x02\x01' and header[18:20] == b'\x3e\x00',
                'candidate must be a Linux amd64 ELF binary')
        endpoint = os.environ.get('DOCKER_HOST') or self.docker(
            'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}').stdout.strip()
        require(endpoint.startswith(('unix://', 'npipe://')), 'rehearsal requires a local Docker endpoint')
        for image in (self.args.runtime_image, self.args.postgres_image, self.args.redis_image):
            self.docker('image', 'inspect', image)
        names = [self.prefix, self.prefix + '-db', self.prefix + '-redis', self.prefix + '-app']
        if getattr(self.args, 'overlap_config', False):
            names.append(self.prefix + '-peer')
        for name in names:
            kind = 'network' if name == self.prefix else 'container'
            require(self.docker(kind, 'inspect', name, check=False).returncode != 0,
                    'existing rehearsal resource requires owner recovery: ' + name)
        require(not self.baseline.exists() and not self.baseline.is_symlink(),
                'scratch baseline already exists; preserve it before retrying')

    def create(self, kind, *args):
        command = ('network', 'create') if kind == 'network' else ('run', '--pull', 'never', '-d')
        identity = self.docker(*command, '--label', LABEL + '=' + self.owner, *args).stdout.strip()
        require(re.fullmatch(r'[0-9a-f]{64}', identity), 'Docker did not return a resource ID')
        self.resources.append((kind, identity))
        return identity

    def cleanup(self):
        while self.resources:
            kind, identity = self.resources.pop()
            try:
                scope = '{{index .Config.Labels "' + LABEL + '"}}' if kind == 'container' else '{{index .Labels "' + LABEL + '"}}'
                inspected = self.docker(kind, 'inspect', '--format', scope, identity, check=False)
                if inspected.returncode == 0:
                    require(inspected.stdout.strip() == self.owner, 'cleanup ownership mismatch')
                    command = ('rm', '-f', '-v', identity) if kind == 'container' else ('network', 'rm', identity)
                    self.docker(*command)
                else:
                    require(bool(re.search(r'No such (?:object|container|network)|network .* not found',
                                           inspected.stderr, re.I)), 'resource state is unknown')
                self.result['cleanup'].append({'kind': kind, 'id': identity, 'removed': True})
            except (RuntimeError, subprocess.SubprocessError):
                self.result['cleanup'].append({'kind': kind, 'id': identity, 'removed': False})

    def stop_app(self, app=None):
        kind, identity = self.resources[-1]
        require(kind == 'container' and identity == (app or self.app), 'application cleanup order changed')
        self.docker('stop', '--time', '20', identity)
        self.docker('rm', '-v', identity)
        self.resources.pop()
        self.result['cleanup'].append({'kind': 'container', 'id': identity, 'removed': True})

    def version(self, binary):
        result = self.docker('run', '--pull', 'never', '--rm', '--network', 'none',
                             '--mount', f'type=bind,source={binary},target=/rehearsal-bin,readonly',
                             self.args.runtime_image, '/rehearsal-bin', '-version')
        return result.stdout + result.stderr

    def sql(self, query):
        return self.docker('exec', '-i', self.db, 'psql', '-X', '-v', 'ON_ERROR_STOP=1',
                           '-U', 'postgres', '-d', 'sub2api_test', '-Atq', input=query).stdout.strip()

    def api(self, method, path, body=None, token=None, *, app=None):
        headers = {'Content-Type': 'application/json'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        result = self.docker('exec', '-i', app or self.app, 'python', '-c', HTTP_CLIENT,
                             input=json.dumps({'method': method, 'path': path, 'headers': headers, 'body': body}))
        response = json.loads(result.stdout)
        require(response['status'] == 200, f'local API {method} {path}: HTTP {response["status"]}')
        data = response['data']
        require(data.get('code', 0) == 0, 'local API rejected ' + path)
        return data.get('data', data)

    def start_app(self, binary, stage, *, name="app"):
        env = {'AUTO_SETUP': 'true', 'DATABASE_HOST': 'db', 'DATABASE_PORT': '5432',
               'DATABASE_USER': 'postgres', 'DATABASE_PASSWORD': self.password,
               'DATABASE_DBNAME': 'sub2api_test', 'DATABASE_SSLMODE': 'disable',
               'REDIS_HOST': 'redis', 'REDIS_PORT': '6379', 'REDIS_DB': '0',
               'ADMIN_EMAIL': 'rehearsal@example.invalid', 'ADMIN_PASSWORD': self.password,
               'JWT_SECRET': self.jwt, 'SERVER_HOST': '0.0.0.0', 'SERVER_PORT': '8080',
               'SERVER_MODE': 'release', 'RUN_MODE': 'standard', 'DATA_DIR': '/app/data', 'TZ': 'UTC'}
        flags = [part for key, value in env.items() for part in ('--env', key + '=' + value)]
        app = self.create('container', '--name', self.prefix + '-' + name, '--network', self.prefix,
                               '--tmpfs', '/app/data', '--workdir', '/app',
                               '--mount', f'type=bind,source={binary},target=/rehearsal-bin,readonly',
                               *flags, self.args.runtime_image, '/rehearsal-bin')
        if name == "app":
            self.app = app
        deadline = time.monotonic() + 120
        while True:
            try:
                self.api('GET', '/health', app=app)
                break
            except (RuntimeError, TimeoutError, ConnectionError):
                state = self.docker('inspect', '--format', '{{.State.Running}}', app).stdout.strip()
                if state != 'true' or time.monotonic() >= deadline:
                    log = self.docker('logs', '--tail', '12', self.app, check=False)
                    message = (log.stdout + log.stderr)[-3000:]
                    for secret in (self.password, self.jwt):
                        message = message.replace(secret, '[redacted]')
                    self.result['startup_log'] = message
                    raise RuntimeError('local application startup failed at ' + stage)
                time.sleep(0.5)
        loaded = self.docker('exec', app, 'python', '-c',
                             "import hashlib; print(hashlib.file_digest(open('/proc/1/exe','rb'),'sha256').hexdigest())").stdout.strip()
        expected = 'baseline_binary_sha256' if Path(binary) == self.baseline else 'candidate_binary_sha256'
        require(loaded == self.result[expected], 'running binary identity mismatch at ' + stage)
        login = self.api('POST', '/api/v1/auth/login', {'email': 'rehearsal@example.invalid', 'password': self.password}, app=app)
        require(bool(login.get('access_token')), 'local login did not return an access token')
        token = login['access_token']
        compliance = self.api('GET', '/api/v1/admin/compliance', token=token, app=app)
        if compliance['required']:
            compliance = self.api('POST', '/api/v1/admin/compliance/accept', {
                'language': 'en', 'phrase': compliance['ack_phrase_en']}, token)
        require(compliance['required'] is False, 'local fixture onboarding did not complete')
        self.result['stages'].append({'stage': stage, 'health': True, 'login': True,
                                     'fixture_onboarding': True, 'container_id': app, 'loaded_sha256': loaded})
        return login['access_token']

    def migrations(self):
        return json.loads(self.sql("SELECT jsonb_object_agg(filename,checksum) FROM schema_migrations"))

    def snapshot(self, account):
        return self.sql(f"""SELECT jsonb_build_object(
          'account',(SELECT jsonb_build_object('credentials',credentials,'extra',extra,
              'concurrency',concurrency,'rate_multiplier',rate_multiplier) FROM accounts WHERE id={account}),
          'monitor',(SELECT jsonb_build_object('config',config,'paused',paused,'revision',revision,
              'pause_generation',pause_generation) FROM account_performance_monitors WHERE account_id={account}))""")

    def exercise_config_overlap(self, config_path, account, original, legacy_fields, token=None):
        overlap = {'scope': 'priority_configuration_and_account_fields', 'status': 'running',
                   'candidate_node': self.app, 'database_container': self.db, 'redis_container': self.redis}
        self.result['config_overlap'] = overlap
        current = self.api('GET', config_path, token=token)
        require('cache_weight' in current, 'candidate does not support the cache-weight compatibility check')
        overlap.update(checked_fields=sorted(current),
                       candidate_only_fields=sorted(set(current) - set(legacy_fields)))
        current.update(cache_weight=17, target_tps=47, preserve_session_affinity=True, enabled=False)
        self.api('PUT', config_path, current, token)
        peer = None

        def verify(expected, stage, load_choices=None):
            stored = json.loads(self.sql("SELECT value FROM settings WHERE key='priority_scheduling_v1'"))
            changed = [key for key, value in expected.items() if stored.get(key) != value and
                       not (key == 'max_load_percent' and load_choices and stored.get(key) in load_choices)]
            if changed:
                overlap.update(status='incompatible', failed_stage=stage, changed_fields=sorted(changed))
                raise RuntimeError('mixed configuration incompatible at ' + stage + ': ' + ','.join(sorted(changed)))
            require(self.snapshot(account) == original, 'overlap changed retained account fields at ' + stage)

        try:
            peer_token = self.start_app(self.baseline, 'baseline_overlap', name='peer')
            peer = self.result['stages'][-1]['container_id']
            require(peer != self.app, 'overlap needs independent application instances')
            overlap['baseline_node'] = peer
            for identity in (peer, self.app):
                require(self.docker('inspect', '--format', '{{.State.Running}}', identity).stdout.strip() == 'true',
                        'both overlap instances must be running')
            legacy = {key: value for key, value in current.items() if key in legacy_fields}
            legacy.pop('cache_weight', None)
            legacy.pop('target_tps', None)
            legacy['max_load_percent'] = 75
            expected = dict(current, max_load_percent=75)
            self.api('PUT', config_path, legacy, peer_token, app=peer)
            verify(expected, 'baseline_save')
            self.api('PUT', config_path, legacy, token)
            verify(expected, 'candidate_legacy_save')
            latest = dict(expected, cache_weight=29, target_tps=53, max_load_percent=77)
            legacy['max_load_percent'] = 73
            with ThreadPoolExecutor(max_workers=2) as pool:
                old_write = pool.submit(self.api, 'PUT', config_path, legacy, peer_token, app=peer)
                new_write = pool.submit(self.api, 'PUT', config_path, latest, token)
                old_write.result()
                new_write.result()
            verify(latest, 'concurrent_saves', (73, 77))
            self.api('PUT', f'/api/v1/admin/accounts/{account}', {'name': 'upgrade-overlap-account'}, peer_token, app=peer)
            require(self.snapshot(account) == original, 'baseline account edit changed retained fields')
            self.api('GET', '/health', app=peer)
            self.api('GET', '/health')
            overlap.update(status='compatible', concurrent_writes=2, checked_fields=sorted(current),
                           candidate_only_fields=sorted(set(current) - set(legacy_fields)), account_fields_preserved=True)
        except (RuntimeError, OSError, subprocess.SubprocessError, ValueError):
            if overlap['status'] != 'incompatible':
                overlap['status'] = 'failed'
            raise
        finally:
            if peer is not None:
                self.stop_app(peer)

    def exercise(self):
        self.create('network', '--internal', self.prefix)
        self.db = self.create('container', '--name', self.prefix + '-db', '--network', self.prefix,
                               '--network-alias', 'db', '--tmpfs', '/var/lib/postgresql:rw,size=1073741824',
                               '--env', 'PGDATA=/var/lib/postgresql/rehearsal',
                               '--env', 'POSTGRES_PASSWORD=' + self.password,
                               '--env', 'POSTGRES_DB=sub2api_test', self.args.postgres_image)
        self.redis = self.create('container', '--name', self.prefix + '-redis', '--network', self.prefix,
                    '--network-alias', 'redis', '--tmpfs', '/data', self.args.redis_image,
                    'redis-server', '--save', '', '--appendonly', 'no')
        deadline = time.monotonic() + 60
        while self.docker('exec', self.db, 'pg_isready', '-U', 'postgres', check=False).returncode:
            require(time.monotonic() < deadline, 'isolated PostgreSQL startup timed out')
            time.sleep(0.5)
        token = self.start_app(self.baseline, 'baseline')
        config_path = '/api/v1/admin/priority-scheduling/config'
        legacy = self.api('GET', config_path, token=token)
        baseline_fields = set(legacy)
        legacy.update(enabled=False, preserve_session_affinity=True)
        self.api('PUT', config_path, legacy, token)
        account = self.api('POST', '/api/v1/admin/accounts', {
            'name': 'upgrade-fixture', 'platform': 'openai', 'type': 'apikey',
            'credentials': {'api_key': 'synthetic-rehearsal-key', 'base_url': 'https://fixture.invalid'},
            'extra': {'rehearsal_marker': self.owner}, 'concurrency': 2,
            'rate_multiplier': 0.12, 'group_rate_multiplier': 1, 'group_ids': []}, token)['id']
        require(isinstance(account, int) and account > 0, 'invalid fixture account ID')
        self.sql(f"INSERT INTO account_performance_monitors(account_id,config,paused,revision,pause_generation) VALUES({account},'{{\"enabled\":false}}',true,1,1)")
        before, original = self.migrations(), self.snapshot(account)
        self.result['before_migrations'] = len(before)
        self.stop_app()
        token = self.start_app(Path(self.args.candidate).resolve(), 'candidate')
        after = self.migrations()
        require(all(after.get(name) == digest for name, digest in before.items()), 'existing migration checksum changed')
        require(self.snapshot(account) == original, 'upgrade changed fixture credentials, pricing or monitor')
        current = self.api('GET', config_path, token=token)
        require(current['preserve_session_affinity'] and current['enabled'] is False, 'legacy policy was not preserved')
        current['target_tps'] = 47
        self.api('PUT', config_path, current, token)
        legacy.pop('target_tps', None)
        self.api('PUT', config_path, legacy, token)
        current = self.api('GET', config_path, token=token)
        if getattr(self.args, 'overlap_config', False):
            self.exercise_config_overlap(config_path, account, original, baseline_fields, token)
            self.api('PUT', config_path, current, token)
        require(current['target_tps'] == 47, 'legacy-shaped save erased the new TPS target')
        require(current['preserve_session_affinity'] and not current['enabled'], 'legacy-shaped save changed policy')
        self.result.update(after_migrations=len(after), old_checksums_preserved=True,
                           fixture_preserved=True, legacy_client_save_preserved_tps=True)
        self.stop_app()
        token = self.start_app(self.baseline, 'rollback')
        self.api('PUT', f'/api/v1/admin/accounts/{account}', {'name': 'upgrade-fixture-rollback'}, token)
        require(self.snapshot(account) == original, 'rollback account edit changed retained fields')
        require(self.migrations() == after, 'rollback rewrote the migrated schema history')
        self.result['rollback_read_write'] = True
        self.stop_app()
        token = self.start_app(Path(self.args.candidate).resolve(), 'candidate_again')
        final = self.api('GET', config_path, token=token)
        require(final['target_tps'] == 47 and final['preserve_session_affinity'] and not final['enabled'],
                're-upgrade lost saved policy')
        require(self.migrations() == after and self.snapshot(account) == original, 're-upgrade was not idempotent')
        self.result['reupgrade_idempotent'] = True
        self.result['passed'] = True


def local_rehearse(args, verify, sha256):
    baseline = Path(args.baseline).resolve()
    metadata = json.loads((baseline / 'release-manifest.json').read_text(encoding='utf-8'))
    require(metadata['repository'] == 'a5116107/ransub2api', 'unexpected baseline repository')
    require(metadata.get('simple') is True, 'baseline rehearsal expects the Linux amd64 release')
    verify(argparse.Namespace(input=str(baseline / 'input'), version=metadata['version'],
                              sha=metadata['sha'], simple=True))
    run = Rehearsal(args)
    try:
        run.preflight()
        run.baseline.parent.mkdir(parents=True, exist_ok=True)
        archive = baseline / 'input' / f"sub2api_{metadata['version']}_linux_amd64.tar.gz"
        with tarfile.open(archive, 'r:gz') as source:
            members = [m for m in source.getmembers() if m.name in ('sub2api', './sub2api')]
            require(len(members) == 1 and members[0].isfile(), 'baseline must contain one regular binary')
            with source.extractfile(members[0]) as binary, run.baseline.open('xb') as dest:
                run.baseline_owned = True
                while data := binary.read(1024 * 1024):
                    dest.write(data)
        run.baseline.chmod(0o755)
        require('commit: ' + metadata['sha'] in run.version(run.baseline), 'baseline binary source mismatch')
        require('commit: ' + args.candidate_sha in run.version(Path(args.candidate).resolve()), 'candidate binary source mismatch')
        run.result.update(baseline_source=metadata['sha'], baseline_version=metadata['version'],
                          baseline_binary_sha256=sha256(run.baseline), candidate_binary_sha256=sha256(Path(args.candidate)))
        run.exercise()
    except (RuntimeError, OSError, subprocess.SubprocessError, ValueError) as error:
        run.result.update(passed=False, error=str(error))
    finally:
        run.cleanup()
        if run.baseline_owned and run.baseline.exists() and all(x['removed'] for x in run.result['cleanup']):
            run.baseline.unlink()
        if any(not x['removed'] for x in run.result['cleanup']):
            run.result['passed'] = False
        Path(args.output).write_text(json.dumps(run.result, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(run.result))
    require(run.result['passed'], 'local rehearsal failed; inspect the result for the remaining observation')
