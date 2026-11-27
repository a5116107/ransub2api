"""Exercise a drained binary upgrade and rollback using disposable local data."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
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

# Small, bounded RFC6455 fixture codec shared by the actual client and server.
# No compression/extensions are negotiated, and payloads never leave the fixture.
WEBSOCKET_IO = r'''import base64,hashlib,secrets,socket,struct
def ws_exact(stream,size):
    value=b''
    while len(value)<size:
        chunk=stream.read(size-len(value))
        if not chunk:raise EOFError('WebSocket peer closed')
        value+=chunk
    return value
def ws_read(stream,masked):
    a,b=ws_exact(stream,2)
    if a&0x70 or not a&0x80 or bool(b&0x80)!=masked:
        raise ValueError('unsupported fixture WebSocket frame')
    size=b&127
    if size==126:size=struct.unpack('!H',ws_exact(stream,2))[0]
    elif size==127:size=struct.unpack('!Q',ws_exact(stream,8))[0]
    if size>1048576 or (a&15>=8 and size>125):raise ValueError('fixture frame too large')
    mask=ws_exact(stream,4) if masked else b''
    data=ws_exact(stream,size)
    if mask:data=bytes(v^mask[i%4] for i,v in enumerate(data))
    return a&15,data
def ws_send(sock,opcode,data,masked):
    size=len(data)
    if size>1048576:raise ValueError('fixture frame too large')
    prefix=bytes([0x80|opcode,(0x80 if masked else 0)|(size if size<126 else 126 if size<65536 else 127)])
    if size>=126:prefix+=struct.pack('!H' if size<65536 else '!Q',size)
    if masked:
        mask=secrets.token_bytes(4);prefix+=mask
        data=bytes(v^mask[i%4] for i,v in enumerate(data))
    sock.sendall(prefix+data)
def ws_accept(key):
    return base64.b64encode(hashlib.sha1((key+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest()).decode()
'''

# This upstream lives on the rehearsal's internal network. It emits no model
# claims and never forwards a request outside the fixture.
REQUEST_UPSTREAM = WEBSOCKET_IO + r'''import json,threading,time
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
lock=threading.Lock()
gate=threading.Event();gate.set()
state={'active':0,'peak':0,'calls':0,'hold_next':False,'partial_usage_next':False,
       'ws_calls':0,'ws_continuations':0,'ws_cancelled':0}
class Handler(BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def reply(self,body):
        data=json.dumps(body).encode()
        self.send_response(200);self.send_header('Content-Type','application/json')
        self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
    def do_GET(self):
        if self.path=='/v1/responses' and self.headers.get('Upgrade','').lower()=='websocket':
            self.websocket();return
        with lock:self.reply(dict(state))
    def websocket(self):
        self.send_response(101);self.send_header('Upgrade','websocket');self.send_header('Connection','Upgrade')
        self.send_header('Sec-WebSocket-Accept',ws_accept(self.headers['Sec-WebSocket-Key']));self.end_headers()
        self.close_connection=True;self.connection.settimeout(35)
        previous=None
        try:
            while True:
                opcode,raw=ws_read(self.rfile,True)
                if opcode==8:
                    ws_send(self.connection,8,raw,False);return
                if opcode==9:ws_send(self.connection,10,raw,False);continue
                if opcode==10:continue
                if opcode!=1:raise ValueError('fixture expected text')
                body=json.loads(raw)
                if body.get('type')!='response.create':raise ValueError('fixture expected response.create')
                if body.get('previous_response_id'):
                    if body['previous_response_id']!=previous:raise ValueError('fixture continuation changed')
                    with lock:state['ws_continuations']+=1
                with lock:state['ws_calls']+=1
                def emit(event):ws_send(self.connection,1,json.dumps(event).encode(),False)
                previous=self.generate(body,emit)
        except (EOFError,OSError,TimeoutError,ValueError):return
    def do_POST(self):
        size=int(self.headers.get('Content-Length','0'))
        body=json.loads(self.rfile.read(size) or b'{}')
        if self.path=='/arm':
            with lock:
                if state['active']:self.send_error(409);return
                state['hold_next']=True;state['partial_usage_next']=body.get('partial_usage') is True;gate.clear()
            self.reply({'armed':True});return
        if self.path=='/release':
            gate.set();self.reply({'released':True});return
        if self.path!='/v1/responses':self.send_error(404);return
        self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
        def emit(event):
            self.wfile.write(('data: '+json.dumps(event)+'\n\n').encode());self.wfile.flush()
        self.generate(body,emit)
    def generate(self,body,emit):
        with lock:
            state['active']+=1;state['calls']+=1
            state['peak']=max(state['peak'],state['active'])
            hold=state['hold_next'];state['hold_next']=False
            partial_usage=state['partial_usage_next'];state['partial_usage_next']=False
            number=state['calls']
        try:
            rid='resp_rehearsal_'+str(number)
            emit({'type':'response.created','response':{'id':rid,'status':'in_progress'}})
            if partial_usage:
                emit({'type':'response.in_progress','response':{'id':rid,'status':'in_progress','model':body['model'],
                      'usage':{'input_tokens':3,'output_tokens':2}}})
            emit({'type':'response.output_text.delta','delta':'fixture OK'})
            deadline=time.monotonic()+30
            while hold and not gate.wait(.1):
                if time.monotonic()>=deadline:raise TimeoutError('fixture release deadline')
                if self.headers.get('Upgrade','').lower()=='websocket':
                    ws_send(self.connection,9,b'fixture heartbeat',False)
                else:
                    self.wfile.write(b': fixture heartbeat\n\n');self.wfile.flush()
            emit({'type':'response.completed','response':{'id':rid,'status':'completed','model':body['model'],
                  'output':[{'type':'message','role':'assistant','content':[{'type':'output_text','text':'fixture OK'}]}],
                  'usage':{'input_tokens':16,'output_tokens':4}}})
            return rid
        except OSError as error:
            if hold and not isinstance(error,TimeoutError) and self.headers.get('Upgrade','').lower()=='websocket':
                with lock:state['ws_cancelled']+=1
        finally:
            with lock:state['active']-=1
ThreadingHTTPServer(('0.0.0.0',8787),Handler).serve_forever()
'''

STREAM_CLIENT = WEBSOCKET_IO + r'''import json,sys
from urllib.request import Request,ProxyHandler,build_opener
from urllib.error import HTTPError,URLError
p=json.load(sys.stdin)
headers={'Content-Type':'application/json','Authorization':'Bearer '+p['key'],
         'session_id':p['case'],'X-Request-ID':p['case']}
request=Request('http://127.0.0.1:8080/v1/responses',headers=headers,
                data=json.dumps({'model':'gpt-6.1-sol','input':p['case'],'stream':True,
                                 'store':False,'max_output_tokens':32}).encode())
result={'status':0,'completed':0,'deltas':0,'errors':0,'cancelled':False,'client_request_id':'','error_code':''}
if p.get('websocket'):
    result.update(close_code=0,response_ids=[],active_response_id='')
    with socket.create_connection(('127.0.0.1',8080),timeout=15) as sock:
        with sock.makefile('rb',buffering=0) as stream:
            key=base64.b64encode(secrets.token_bytes(16)).decode()
            raw=('GET /v1/responses HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nUpgrade: websocket\r\n'
                 'Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: '+key+'\r\n'
                 'Authorization: Bearer '+p['key']+'\r\nSession_id: '+p['case']+'\r\nX-Request-ID: '+p['case']+'\r\n\r\n')
            sock.sendall(raw.encode())
            status=stream.readline(8192).decode().split();result['status']=int(status[1])
            received={};total=0
            while True:
                line=stream.readline(8192);total+=len(line)
                if total>65536 or not line:raise ValueError('invalid fixture handshake')
                if line==b'\r\n':break
                name,value=line.decode().split(':',1);received[name.lower()]=value.strip()
            result['client_request_id']=received.get('x-client-request-id','')
            if result['status']!=101:raise ValueError('fixture WebSocket upgrade failed')
            if received.get('sec-websocket-accept')!=ws_accept(key):raise ValueError('invalid fixture WebSocket accept')
            for turn in range(p.get('turns',1)):
                body={'type':'response.create','model':'gpt-6.1-sol','input':p['case'],'store':False,'max_output_tokens':32}
                if result['response_ids']:body['previous_response_id']=result['response_ids'][-1]
                ws_send(sock,1,json.dumps(body).encode(),True)
                for _ in range(100):
                    opcode,raw=ws_read(stream,False)
                    if opcode==8:
                        result['close_code']=struct.unpack('!H',raw[:2])[0] if len(raw)>=2 else 1005
                        break
                    if opcode==9:ws_send(sock,10,raw,True);continue
                    if opcode==10:continue
                    if opcode!=1:raise ValueError('fixture expected text event')
                    event=json.loads(raw);kind=event.get('type')
                    if kind in ('response.created','response.in_progress'):
                        result['active_response_id']=event.get('response',{}).get('id','')
                    if kind in ('response.failed','response.incomplete','error'):
                        result['errors']+=1
                        result['error_code']=event.get('error',{}).get('code','')
                    if kind=='response.output_text.delta' and event.get('delta'):
                        result['deltas']+=1
                        if p.get('cancel'):
                            result['cancelled']=True;break
                    if kind=='response.completed':
                        result['completed']+=1;result['response_ids'].append(event['response']['id']);break
                else:raise ValueError('fixture WebSocket terminal absent')
                if result['close_code'] or result['cancelled']:break
            if p.get('cancel')=='abort':sock.shutdown(socket.SHUT_RDWR)
            elif not result['close_code']:ws_send(sock,8,struct.pack('!H',1000),True)
    print(json.dumps(result));raise SystemExit(0)
try:
    with build_opener(ProxyHandler({})).open(request,timeout=15) as response:
        result.update(status=response.status,client_request_id=response.headers.get('x-client-request-id',''))
        for line in response:
            if not line.startswith(b'data:'):continue
            event=json.loads(line[5:]);kind=event.get('type')
            if kind=='response.completed':result['completed']+=1
            if kind in ('response.failed','response.incomplete','error'):result['errors']+=1
            if kind=='response.output_text.delta' and event.get('delta'):
                result['deltas']+=1
                if p.get('cancel'):
                    result['cancelled']=True;break
except HTTPError as error:
    result.update(status=error.code,client_request_id=error.headers.get('x-client-request-id',''))
    try:result['error_code']=json.loads(error.read(65536)).get('error',{}).get('code','')
    except (ValueError,AttributeError):pass
except (URLError,TimeoutError,ConnectionError):result['status']=0
print(json.dumps(result))
'''

MIXED_KEY = 'synthetic-rehearsal-shared-upstream-key'


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
        if getattr(self.args, 'overlap_config', False) or getattr(self.args, 'overlap_requests', False):
            names.append(self.prefix + '-peer')
        if getattr(self.args, 'overlap_requests', False):
            names.append(self.prefix + '-upstream')
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
        command = ['/rehearsal-bin']
        if getattr(self.args, 'overlap_requests', False):
            policy = {'gateway': {'openai_ws': {'mode_router_v2_enabled': True, 'apikey_enabled': True},
                                  'upstream_key_concurrency': {'revision': 1, 'limits': {
                hashlib.sha256(MIXED_KEY.encode()).hexdigest(): 1}}}}
            env['CONFIG_FILE'] = '/app/data/rehearsal.yaml'
            launcher = ("import os,pathlib;pathlib.Path('/app/data/rehearsal.yaml').write_text("
                        + repr(json.dumps(policy)) + ");os.execv('/rehearsal-bin',['/rehearsal-bin'])")
            command = ['python', '-c', launcher]
        flags = [part for key, value in env.items() for part in ('--env', key + '=' + value)]
        app = self.create('container', '--name', self.prefix + '-' + name, '--network', self.prefix,
                               '--tmpfs', '/app/data', '--workdir', '/app',
                               '--mount', f'type=bind,source={binary},target=/rehearsal-bin,readonly',
                               *flags, self.args.runtime_image, *command)
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
                    log = self.docker('logs', '--tail', '12', app, check=False)
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
                'language': 'en', 'phrase': compliance['ack_phrase_en']}, token, app=app)
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

    def fixture_api(self, path, body=None):
        client = HTTP_CLIENT.replace('127.0.0.1:8080', '127.0.0.1:8787')
        reply = self.docker('exec', '-i', self.upstream, 'python', '-c', client,
                            input=json.dumps({'method': 'GET' if body is None else 'POST',
                                              'path': path, 'headers': {'Content-Type': 'application/json'}, 'body': body}))
        response = json.loads(reply.stdout)
        require(response['status'] == 200, 'isolated upstream control failed at ' + path)
        return response['data']

    def stream_request(self, node, key, case, *, cancel=False, websocket=False, turns=1):
        payload = {'key': key, 'case': case, 'cancel': cancel}
        if websocket:
            payload.update(websocket=True, turns=turns)
        reply = self.docker('exec', '-i', node, 'python', '-c', STREAM_CLIENT,
                            input=json.dumps(payload))
        return json.loads(reply.stdout)

    def wait_request_state(self, *, active, slots, calls=None):
        fingerprint = hashlib.sha256(MIXED_KEY.encode()).hexdigest()
        deadline = time.monotonic() + 15
        while True:
            state = self.fixture_api('/stats')
            held = int(self.docker('exec', self.redis, 'redis-cli', '--raw', 'ZCARD',
                                  'concurrency:upstream_key:slots:' + fingerprint).stdout.strip())
            if state['active'] == active and held == slots and (calls is None or state['calls'] == calls):
                return state
            require(time.monotonic() < deadline, 'mixed request/slot state did not converge: '
                    + json.dumps({'active': state['active'], 'slots': held, 'calls': state['calls'],
                                  'expected_active': active, 'expected_slots': slots, 'expected_calls': calls}))
            time.sleep(.1)

    def verify_stream_settlement(self, response, account, group, multiplier):
        require(response['status'] == 200 and response['completed'] == 1 and response['deltas'] > 0
                and response['errors'] == 0 and not response['cancelled'], 'mixed stream did not complete exactly once')
        return self.verify_request_settlement(response['client_request_id'], account, group, multiplier)

    def verify_request_settlement(self, request_id, account, group, multiplier, *, websocket=False,
                                  input_tokens=16, output_tokens=4):
        require(re.fullmatch(r'[A-Za-z0-9_-]{1,255}', request_id), 'invalid mixed-stream request identity')
        billing_id = request_id if websocket else 'client:' + request_id
        query = f"""SELECT jsonb_build_object('usage',count(*),'account',min(u.account_id),'group',min(u.group_id),
            'rate',min(u.rate_multiplier),'input_tokens',min(u.input_tokens),'output_tokens',min(u.output_tokens),
            'claims',(SELECT count(*) FROM usage_billing_dedup WHERE request_id='{billing_id}'),
            'quotes',(SELECT count(*) FROM usage_billing_quotes WHERE request_id='{billing_id}'),
            'canonical_matches',bool_and(q.canonical_actual_cost=u.actual_cost),
            'debit_matches',bool_and(q.wallet_debit=u.actual_cost AND q.debited_user_id=u.user_id))
            FROM usage_logs u LEFT JOIN usage_billing_quotes q
            ON q.request_id=u.request_id AND q.api_key_id=u.api_key_id WHERE u.request_id='{billing_id}'"""
        deadline = time.monotonic() + 15
        while True:
            observed = json.loads(self.sql(query))
            if observed['usage']:
                break
            require(time.monotonic() < deadline, 'mixed stream usage was not persisted')
            time.sleep(.1)
        require(observed['usage'] == observed['claims'] == observed['quotes'] == 1,
                'mixed stream usage/claim/quote was not unique')
        require(observed['account'] == account and observed['group'] == group
                and observed['rate'] == multiplier, 'mixed stream changed account/group or selling rate')
        require(observed['canonical_matches'] and observed['debit_matches'], 'mixed stream debit attribution mismatch')
        require(observed['input_tokens'] == input_tokens and observed['output_tokens'] == output_tokens,
                'mixed stream billed usage differs from the actual current upstream turn')
        return observed

    def exercise_websocket_overlap(self, peer, keys, accounts, groups, rates):
        proof = {'scope': 'native_responses_websocket_capacity_continuation_and_settlement',
                 'status': 'running', 'cases': [], 'shared_key_limit': 1}
        self.result['websocket_overlap'] = proof
        row_counts = "SELECT jsonb_build_object('usage',(SELECT count(*) FROM usage_logs),'claims',(SELECT count(*) FROM usage_billing_dedup),'quotes',(SELECT count(*) FROM usage_billing_quotes))"
        try:
            before_rows = json.loads(self.sql(row_counts))
            for index, (first, second) in enumerate(((peer, self.app), (self.app, peer))):
                for holder_ws, contender_ws in ((True, True), (True, False), (False, True)):
                    before = self.wait_request_state(active=0, slots=0)['calls']
                    case = f'mixed-ws-{index}-{int(holder_ws)}-{int(contender_ws)}'
                    self.fixture_api('/arm', {})
                    with ThreadPoolExecutor(max_workers=1) as pool:
                        pending = pool.submit(self.stream_request, first, keys[index], case, websocket=holder_ws)
                        try:
                            self.wait_request_state(active=1, slots=1, calls=before + 1)
                            rejected = self.stream_request(second, keys[1-index], case+'-full', websocket=contender_ws)
                            if contender_ws:
                                require(rejected['status'] == 101 and rejected['close_code'] == 1013
                                        and not rejected['response_ids'] and rejected['deltas'] == 0,
                                        'mixed WS saturation did not close without output')
                            else:
                                require(rejected['status'] == 503 and rejected['error_code'] == 'upstream_key_admission_unavailable',
                                        'mixed HTTP saturation lost admission reason')
                            self.wait_request_state(active=1, slots=1, calls=before + 1)
                        finally:
                            self.fixture_api('/release', {})
                        completed = pending.result(timeout=20)
                    if holder_ws:
                        require(completed['status'] == 101 and completed['completed'] == 1 and completed['deltas'] > 0
                                and not completed['errors'] and not completed['close_code'], 'native WS holder failed')
                        self.verify_request_settlement(completed['response_ids'][0], accounts[index], groups[index], rates[index], websocket=True)
                    else:
                        self.verify_stream_settlement(completed, accounts[index], groups[index], rates[index])
                    self.wait_request_state(active=0, slots=0, calls=before + 1)
                    recovered = self.stream_request(second, keys[1-index], case+'-recovery', websocket=True, turns=2)
                    require(recovered['status'] == 101 and recovered['completed'] == 2 and recovered['deltas'] >= 2
                            and not recovered['errors'] and not recovered['close_code']
                            and len(set(recovered['response_ids'])) == 2, 'native WS continuation did not complete two unique turns')
                    for response_id in recovered['response_ids']:
                        self.verify_request_settlement(response_id, accounts[1-index], groups[1-index], rates[1-index], websocket=True)
                    state = self.wait_request_state(active=0, slots=0, calls=before + 3)
                    require(state['peak'] == 1, 'mixed WS/HTTP doubled physical generation')
                    proof['cases'].append({'direction': 'baseline_to_candidate' if index == 0 else 'candidate_to_baseline',
                        'holder': 'ws' if holder_ws else 'http', 'contender': 'ws' if contender_ws else 'http',
                        'rejected_sends': 0, 'held_unique_settlement': True, 'recovery_unique_turns': 2})
            proof.update(status='compatible', successful_turns=18, rejected_requests=6,
                         continuation_roundtrips=state['ws_continuations'], peak_physical_concurrency=state['peak'])
            require(state['ws_calls'] == 16 and state['ws_continuations'] == 6, 'native WS evidence missing or contaminated by HTTP fallback')
            after_rows = json.loads(self.sql(row_counts))
            require(all(after_rows[k] - before_rows[k] == 18 for k in before_rows),
                    'mixed WS admission rejection or retry created extra billing rows')
            proof['rejection_billing_rows'] = 0
            proof['cancellations'] = []
            cancellation_rows = json.loads(self.sql(row_counts))
            for index, (first, second) in enumerate(((peer, self.app), (self.app, peer))):
                for close_mode in ('normal', 'abort'):
                    before = self.wait_request_state(active=0, slots=0)['calls']
                    case = f'mixed-ws-cancel-{index}-{close_mode}'
                    attempt = {'direction': 'baseline_to_candidate' if index == 0 else 'candidate_to_baseline',
                               'close_mode': close_mode, 'status': 'running'}
                    proof['cancellations'].append(attempt)
                    self.fixture_api('/arm', {'partial_usage': True})
                    try:
                        cancelled = self.stream_request(first, keys[index], case, websocket=True,
                                                        cancel=True if close_mode == 'normal' else 'abort')
                        require(cancelled['status'] == 101 and cancelled['cancelled'] and cancelled['completed'] == 0
                                and cancelled['deltas'] > 0 and not cancelled['response_ids'],
                                'native WS cancellation accepted a terminal or never received output')
                        ended = self.wait_request_state(active=0, slots=0, calls=before + 1)
                        require(ended['ws_cancelled'] == len(proof['cancellations']),
                                'native WS cancellation did not end the physical upstream generation')
                        attempt.update(downstream_completed=False, physical_upstream_ended=True,
                                       slots_released=True, replayed=False)
                    finally:
                        self.fixture_api('/release', {})
                    partial = self.verify_request_settlement(cancelled['active_response_id'], accounts[index],
                            groups[index], rates[index], websocket=True, input_tokens=3, output_tokens=2)
                    recovered = self.stream_request(second, keys[1-index], case+'-recovery', websocket=True)
                    require(recovered['status'] == 101 and recovered['completed'] == 1 and recovered['deltas'] > 0
                            and not recovered['errors'] and not recovered['close_code'],
                            'peer did not recover after the native WS cancellation')
                    self.verify_request_settlement(recovered['response_ids'][0], accounts[1-index],
                                                   groups[1-index], rates[1-index], websocket=True)
                    state = self.wait_request_state(active=0, slots=0, calls=before + 2)
                    require(state['peak'] == 1, 'native WS cancellation/recovery exceeded physical capacity')
                    attempt.update(status='compatible', partial_settlement=partial, recovery_unique_settlement=True)
            after_rows = json.loads(self.sql(row_counts))
            require(all(after_rows[k] - cancellation_rows[k] == 8 for k in cancellation_rows),
                    'native WS cancellation/recovery duplicated or lost a partial usage/claim/quote')
            require(state['ws_calls'] == 24 and state['ws_cancelled'] == 4,
                    'native WS cancellation fell back to HTTP or failed to terminate')
            proof.update(cancelled_turns=4, cancellation_recovery_turns=4, partial_usage_unique=True)
        except (RuntimeError, OSError, subprocess.SubprocessError, ValueError):
            proof['status'] = 'failed'
            for attempt in proof.get('cancellations', []):
                if attempt['status'] == 'running':
                    attempt['status'] = 'failed'
            raise

    def exercise_request_overlap(self, token):
        overlap = {'scope': 'responses_http_shared_key_capacity_cancellation_and_settlement', 'status': 'running',
                   'candidate_node': self.app, 'database_container': self.db, 'redis_container': self.redis,
                   'upstream_container': self.upstream, 'shared_key_limit': 1, 'cases': []}
        self.result['request_overlap'] = overlap
        peer = None
        try:
            self.start_app(self.baseline, 'baseline_requests', name='peer')
            peer = self.result['stages'][-1]['container_id']
            overlap['baseline_node'] = peer
            require(peer != self.app, 'request overlap needs independent application instances')
            for node in (peer, self.app):
                require(self.docker('inspect', '--format', '{{.State.Running}}', node).stdout.strip() == 'true',
                        'both mixed-request instances must be running')
            self.sql("UPDATE users SET balance=100, concurrency=16 WHERE email='rehearsal@example.invalid'")
            accounts, groups, keys, rates = [], [], [], [.12, .15]
            for index, rate in enumerate(rates):
                group = self.api('POST', '/api/v1/admin/groups', {'name': 'mixed-requests-' + str(index),
                                 'platform': 'openai', 'rate_multiplier': rate, 'subscription_type': 'standard'}, token)['id']
                account = self.api('POST', '/api/v1/admin/accounts', {
                    'name': 'mixed-key-' + str(index), 'platform': 'openai', 'type': 'apikey',
                    'credentials': {'api_key': MIXED_KEY, 'base_url': 'http://upstream:8787/v1'},
                    'extra': {'openai_responses_mode': 'force_responses',
                              'openai_apikey_responses_websockets_v2_enabled': True,
                              'openai_apikey_responses_websockets_v2_mode': 'passthrough'},
                    'concurrency': 2, 'rate_multiplier': 1, 'group_ids': [group]}, token)['id']
                key = self.api('POST', '/api/v1/keys', {'name': 'mixed-' + str(index), 'group_id': group}, token)['key']
                accounts.append(account); groups.append(group); keys.append(key)
            target = int(self.sql('SELECT COALESCE(MAX(id),0) FROM scheduler_outbox'))
            deadline = time.monotonic() + 30
            while int(self.docker('exec', self.redis, 'redis-cli', '--raw', 'GET',
                                  'sched:outbox:watermark').stdout.strip() or '0') < target:
                require(time.monotonic() < deadline, 'mixed request account snapshot was not consumed')
                time.sleep(.2)
            rejected_ids = []
            for index, (first, second) in enumerate(((peer, self.app), (self.app, peer))):
                before = self.wait_request_state(active=0, slots=0)['calls']
                self.fixture_api('/arm', {})
                with ThreadPoolExecutor(max_workers=1) as pool:
                    pending = pool.submit(self.stream_request, first, keys[index], 'mixed-held-' + str(index))
                    try:
                        try:
                            self.wait_request_state(active=1, slots=1, calls=before + 1)
                        except RuntimeError:
                            if pending.done():
                                overlap['held_request_failure'] = pending.result()
                            raise
                        rejected = self.stream_request(second, keys[1-index], 'mixed-full-' + str(index))
                        require(rejected['status'] == 503 and rejected['error_code'] == 'upstream_key_admission_unavailable',
                                'mixed shared-key saturation did not return its local admission reason')
                        rejected_ids.append(rejected['client_request_id'])
                        state = self.wait_request_state(active=1, slots=1, calls=before + 1)
                        require(state['peak'] == 1, 'mixed version doubled shared upstream capacity')
                    finally:
                        self.fixture_api('/release', {})
                    completed = pending.result(timeout=20)
                settlement = self.verify_stream_settlement(completed, accounts[index], groups[index], rates[index])
                self.wait_request_state(active=0, slots=0, calls=before + 1)
                recovered = self.stream_request(second, keys[1-index], 'mixed-recovery-' + str(index))
                recovery = self.verify_stream_settlement(recovered, accounts[1-index], groups[1-index], rates[1-index])
                self.wait_request_state(active=0, slots=0, calls=before + 2)
                overlap['cases'].append({'direction': 'baseline_to_candidate' if index == 0 else 'candidate_to_baseline',
                    'rejected_status': rejected['status'], 'rejected_physical_sends': 0,
                    'held_settlement': settlement, 'recovery_settlement': recovery})
            overlap['cancellations'] = []
            for index, (first, second) in enumerate(((peer, self.app), (self.app, peer))):
                before = self.wait_request_state(active=0, slots=0)['calls']
                self.fixture_api('/arm', {})
                try:
                    cancelled = self.stream_request(first, keys[index], 'mixed-cancel-' + str(index), cancel=True)
                    require(cancelled['status'] == 200 and cancelled['cancelled'] and cancelled['completed'] == 0
                            and cancelled['deltas'] > 0, 'mixed cancellation did not close an active stream')
                    # HTTP deliberately drains upstream usage after downstream
                    # cancellation. Keep capacity until that physical call ends.
                    self.wait_request_state(active=1, slots=1, calls=before + 1)
                    rejected = self.stream_request(second, keys[1-index], 'mixed-cancel-full-' + str(index))
                    require(rejected['status'] == 503 and rejected['error_code'] == 'upstream_key_admission_unavailable',
                            'mixed cancellation released capacity before the upstream ended')
                    rejected_ids.append(rejected['client_request_id'])
                    self.wait_request_state(active=1, slots=1, calls=before + 1)
                finally:
                    self.fixture_api('/release', {})
                self.wait_request_state(active=0, slots=0, calls=before + 1)
                drained = self.verify_request_settlement(cancelled['client_request_id'], accounts[index], groups[index], rates[index])
                recovered = self.stream_request(second, keys[1-index], 'mixed-after-cancel-' + str(index))
                settlement = self.verify_stream_settlement(recovered, accounts[1-index], groups[1-index], rates[1-index])
                state = self.wait_request_state(active=0, slots=0, calls=before + 2)
                overlap['cancellations'].append({'direction': 'baseline_to_candidate' if index == 0 else 'candidate_to_baseline',
                    'downstream_completed': False, 'held_until_upstream_terminal': True, 'replayed': False,
                    'drained_settlement': drained, 'recovery_settlement': settlement})
            require(state['peak'] == 1, 'mixed cancellation/recovery exceeded shared capacity')
            for request_id in rejected_ids:
                require(re.fullmatch(r'[A-Za-z0-9_-]{1,255}', request_id), 'invalid rejected-request identity')
                billing = json.loads(self.sql(f"""SELECT jsonb_build_object(
                    'usage',(SELECT count(*) FROM usage_logs WHERE request_id='client:{request_id}'),
                    'claims',(SELECT count(*) FROM usage_billing_dedup WHERE request_id='client:{request_id}'),
                    'quotes',(SELECT count(*) FROM usage_billing_quotes WHERE request_id='client:{request_id}'))"""))
                require(all(count == 0 for count in billing.values()), 'local admission rejection created usage or a charge')
            overlap.update(status='compatible', peak_physical_concurrency=state['peak'],
                           cancellation_released_after_terminal=True, rejection_billing_rows=0,
                           successful_streams=6, drained_cancelled_calls=2, rejected_requests=len(rejected_ids))
            self.exercise_websocket_overlap(peer, keys, accounts, groups, rates)
        except (RuntimeError, OSError, subprocess.SubprocessError, ValueError):
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
        if getattr(self.args, 'overlap_requests', False):
            self.upstream = self.create('container', '--name', self.prefix + '-upstream', '--network', self.prefix,
                                        '--network-alias', 'upstream', self.args.runtime_image,
                                        'python', '-u', '-c', REQUEST_UPSTREAM)
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
        if getattr(self.args, 'overlap_requests', False):
            self.exercise_request_overlap(token)
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
