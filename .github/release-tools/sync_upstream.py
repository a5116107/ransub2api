#!/usr/bin/env python3
"""Track upstream stable releases and propose integration without replacing custom code."""
import argparse
import json
import re
import subprocess
from github_api import github

REPOSITORY = 'a5116107/ransub2api'
UPSTREAM = 'ranxi2001/sub2api'
BASE = 'production'
TRACKING = 'upstream/stable'


def command(*args, check=True):
    return subprocess.run(args, text=True, encoding='utf-8', capture_output=True, check=check)


def git(*args, check=True):
    return command('git', *args, check=check)


def gh_json(*args):
    if args[0] == 'api':
        return github('GET', args[1])
    if args[:2] == ('pr', 'list'):
        return github('GET', f'repos/{REPOSITORY}/pulls?state=open&base={BASE}&head=a5116107:{TRACKING}')
    return json.loads(command('gh', *args).stdout)


def integration_status(base, source):
    if git('merge-base', '--is-ancestor', source, base, check=False).returncode == 0:
        return 'integrated'
    merged = git('merge-tree', '--write-tree', base, source, check=False)
    if merged.returncode not in (0, 1):
        raise RuntimeError('merge preview failed; no branch or release was changed')
    return 'ready_for_review' if merged.returncode == 0 else 'conflicts'


def sync(dry_run=False):
    repository = gh_json('api', f'repos/{REPOSITORY}')
    if repository['full_name'] != REPOSITORY or repository['default_branch'] != BASE:
        raise ValueError('unexpected repository identity or default branch')
    origin = git('remote', 'get-url', '--push', 'origin').stdout.strip()
    if origin not in (f'https://github.com/{REPOSITORY}.git', f'git@github.com:{REPOSITORY}.git'):
        raise ValueError('origin is not the confirmed user-owned repository')
    release = gh_json('api', f'repos/{UPSTREAM}/releases/latest')
    tag = release['tag_name']
    if release.get('draft') or release.get('prerelease') or not re.fullmatch(r'v\d+\.\d+\.\d+', tag):
        raise ValueError('upstream did not provide a stable version tag')
    git('fetch', '--no-tags', 'origin', BASE)
    base = git('rev-parse', 'FETCH_HEAD').stdout.strip()
    git('fetch', '--no-tags', f'https://github.com/{UPSTREAM}.git', f'refs/tags/{tag}')
    source = git('rev-parse', 'FETCH_HEAD^{commit}').stdout.strip()
    status = integration_status(base, source)
    result = {'repository': REPOSITORY, 'upstream': UPSTREAM, 'tag': tag,
              'base_sha': base, 'upstream_sha': source, 'status': status, 'dry_run': dry_run}
    if dry_run or status == 'integrated':
        return result
    # Normal push intentionally rejects a rewritten upstream history. Never
    # force-push or copy upstream files over the custom production branch.
    git('push', 'origin', f'{source}:refs/heads/{TRACKING}')
    title = f'chore: integrate upstream {tag} [skip ci]'
    body = (f'Upstream: {UPSTREAM} {tag}\n\n'
            f'Pinned upstream commit: `{source}`\n\n'
            f'Custom base at preview: `{base}`\n\n'
            f'Merge preview: **{status}**.\n\n'
            'This sync does not merge production, build, release, or deploy. '
            'Retain custom scheduling, session/cache ownership, shared-Key limits, '
            'regional video ownership, and account performance monitoring. '
            'Resolve semantic conflicts, run local regressions and migration/rollback '
            'checks, then publish a custom stable release. No CI was requested.\n')
    existing = gh_json('pr', 'list', '--repo', REPOSITORY, '--base', BASE,
                       '--head', TRACKING, '--state', 'open', '--json', 'number,url')
    if existing:
        github('PATCH', f'repos/{REPOSITORY}/pulls/{existing[0]["number"]}', {'title': title, 'body': body})
        result['pull_request'] = existing[0]['html_url']
    else:
        request = github('POST', f'repos/{REPOSITORY}/pulls', {'title': title, 'body': body, 'base': BASE, 'head': TRACKING})
        result['pull_request'] = request['html_url']
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dry-run', action='store_true')
    print(json.dumps(sync(parser.parse_args().dry_run), ensure_ascii=False))
