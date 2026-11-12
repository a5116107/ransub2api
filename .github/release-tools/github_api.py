"""Small GitHub API client shared by sync and local release publication."""
import json
import os
import subprocess
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import ProxyHandler, Request, build_opener


def github(method, path, data=None, *, upload=None):
    token = os.environ.get('GH_TOKEN') or os.environ.get('GITHUB_TOKEN')
    if not token:
        token = subprocess.check_output(['gh', 'auth', 'token'], text=True, encoding='utf-8').strip()
    url = path if path.startswith('https://') else 'https://api.github.com/' + path.lstrip('/')
    if urlsplit(url).netloc not in ('api.github.com', 'uploads.github.com'):
        raise ValueError('unexpected GitHub API authority')
    headers = {'Authorization': 'Bearer ' + token, 'Accept': 'application/vnd.github+json',
               'X-GitHub-Api-Version': '2022-11-28', 'User-Agent': 'RanSub2API-Release',
               'Connection': 'close'}
    body = None
    if upload is not None:
        body = upload.read_bytes()
        headers['Content-Type'] = 'application/octet-stream'
    elif data is not None:
        body = json.dumps(data).encode('utf-8')
        headers['Content-Type'] = 'application/json'
    request = Request(url, data=body, headers=headers, method=method)
    try:
        # Explicit direct connections avoid a Windows system HTTP proxy being
        # applied to a SOCKS listener. Each request closes its connection.
        with build_opener(ProxyHandler({})).open(request, timeout=120 if upload else 30) as response:
            result = response.read()
    except HTTPError as error:
        raise RuntimeError(f'GitHub {method} {urlsplit(url).path} returned {error.code}') from None
    return json.loads(result) if result else None
