#!/usr/bin/env python3
"""Track upstream stable releases and propose integration without replacing custom code."""
import argparse
import json
import os
import re
import subprocess
import tempfile
from github_api import github

REPOSITORY = 'a5116107/ransub2api'
UPSTREAM = 'ranxi2001/sub2api'
BASE = 'production'
TRACKING = 'upstream/stable'
INTEGRATION = 'integration/upstream-stable'


def command(*args, check=True, env=None):
    result = subprocess.run(args, text=True, encoding='utf-8', capture_output=True, check=False, env=env)
    if check and result.returncode:
        detail = result.stderr or result.stdout or 'no diagnostic output'
        for name in ('GH_TOKEN', 'GITHUB_TOKEN'):
            if os.environ.get(name):
                detail = detail.replace(os.environ[name], '[REDACTED]')
        detail = re.sub(r'https?://[^\s/@]+(?::[^\s/@]+)?@', 'https://[REDACTED]@', detail)
        detail = re.sub(r'\b(?:gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+)', '[REDACTED]', detail)
        raise RuntimeError(f'{args[0]} {args[1] if len(args) > 1 else ""} failed '
                           f'(exit {result.returncode}): {detail[-6000:].strip()}')
    return result


def git(*args, check=True, env=None):
    return command('git', *args, check=check, env=env)


def gh_json(*args):
    if args[0] == 'api':
        return github('GET', args[1])
    if args[:2] == ('pr', 'list'):
        head = args[args.index('--head') + 1]
        return github('GET', f'repos/{REPOSITORY}/pulls?state=open&base={BASE}&head=a5116107:{head}')
    return json.loads(command('gh', *args).stdout)


def integration_status(base, source):
    if git('merge-base', '--is-ancestor', source, base, check=False).returncode == 0:
        return 'integrated'
    merged = git('merge-tree', '--write-tree', base, source, check=False)
    if merged.returncode not in (0, 1):
        raise RuntimeError('merge preview failed; no branch or release was changed')
    return 'ready_for_review' if merged.returncode == 0 else 'conflicts'


def tracking_candidate(base, source, tag, bootstrap=False):
    """Mirror application sources while retaining the owner's workflow policy.

    The merge commit retains upstream ancestry and records the exact raw source.
    Only .github/workflows differs from upstream. An alternate index keeps the
    developer's checkout and index untouched, including on command failure.
    """
    git('fetch', '--no-tags', 'origin', f'refs/heads/{TRACKING}')
    previous = git('rev-parse', 'FETCH_HEAD^{commit}').stdout.strip()
    message = git('show', '-s', '--format=%B', previous).stdout
    recorded = re.search(r'^Upstream-Commit: ([0-9a-f]{40})$', message, re.MULTILINE)
    prior_source = recorded[1] if recorded else previous
    if git('merge-base', '--is-ancestor', prior_source, source, check=False).returncode:
        raise ValueError('upstream history is not fast-forward; tracking branch retained')
    if not recorded and not bootstrap:
        raise ValueError('tracking workflow policy needs one local --bootstrap-tracking run')
    if recorded and prior_source == source:
        if git('diff', '--quiet', source, previous, '--', '.', ':!.github/workflows', check=False).returncode:
            raise ValueError('tracking branch contains non-upstream application changes')
        return previous
    policy = base if bootstrap else previous
    with tempfile.TemporaryDirectory(prefix='ransub-sync-') as directory:
        env = dict(os.environ, GIT_INDEX_FILE=os.path.join(directory, 'index'),
                   GIT_AUTHOR_NAME='RanSub2API Sync', GIT_AUTHOR_EMAIL='sync@users.noreply.github.com',
                   GIT_COMMITTER_NAME='RanSub2API Sync', GIT_COMMITTER_EMAIL='sync@users.noreply.github.com')
        git('read-tree', source, env=env)
        # --cached only changes the disposable alternate index, never files.
        git('rm', '-r', '-f', '--cached', '--ignore-unmatch', '.github/workflows', env=env)
        git('read-tree', '--prefix=.github/workflows/', f'{policy}:.github/workflows', env=env)
        tree = git('write-tree', env=env).stdout.strip()
        parents = ['-p', previous] + (['-p', source] if previous != source else [])
        candidate = git('commit-tree', tree, *parents, '-m',
                        f'chore: track upstream {tag} with owner workflows [skip ci]\n\n'
                        f'Upstream-Commit: {source}', env=env).stdout.strip()
    if git('diff', '--quiet', source, candidate, '--', '.', ':!.github/workflows', check=False).returncode:
        raise ValueError('tracking snapshot differs from upstream application sources')
    if git('diff', '--quiet', policy, candidate, '--', '.github/workflows', check=False).returncode:
        raise ValueError('tracking snapshot changed the owner workflow policy')
    return candidate


def review_candidate(base, source, status):
    """Reuse a maintainer-resolved branch; never replace it with an upstream tip."""
    remote = git('ls-remote', '--heads', 'origin', f'refs/heads/{INTEGRATION}').stdout.strip()
    if not remote:
        return TRACKING, source, status, True
    git('fetch', '--no-tags', 'origin', f'refs/heads/{INTEGRATION}')
    candidate = git('rev-parse', 'FETCH_HEAD^{commit}').stdout.strip()
    contains_source = git('merge-base', '--is-ancestor', source, candidate, check=False).returncode == 0
    if not contains_source:
        # A previously merged candidate has nothing left to review. Propose the
        # new tracking tip rather than opening an empty PR from that ancestor.
        if git('merge-base', '--is-ancestor', candidate, base, check=False).returncode == 0:
            return TRACKING, source, status, True
        return INTEGRATION, candidate, 'requires_integration', False
    return INTEGRATION, candidate, integration_status(base, candidate), True


def sync(dry_run=False, bootstrap_tracking=False):
    repository = gh_json('api', f'repos/{REPOSITORY}')
    if repository['full_name'] != REPOSITORY or repository['default_branch'] != BASE:
        raise ValueError('unexpected repository identity or default branch')
    origin = git('remote', 'get-url', '--push', 'origin').stdout.strip()
    if origin.removesuffix('.git') not in (f'https://github.com/{REPOSITORY}', f'git@github.com:{REPOSITORY}'):
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
    # The workflow's ephemeral repository token suppresses recursive Actions.
    # A local user token would run workflows contained in the upstream commit.
    if os.environ.get('GITHUB_ACTIONS') != 'true' and not bootstrap_tracking:
        raise ValueError('use workflow_dispatch for synchronization; local runs support --dry-run only')
    tracked = tracking_candidate(base, source, tag, bootstrap=bootstrap_tracking)
    git('push', 'origin', f'{tracked}:refs/heads/{TRACKING}')
    status = integration_status(base, tracked)
    head, candidate, status, contains_source = review_candidate(base, tracked, status)
    result['tracking_sha'] = tracked
    result.update(status=status, review_head=head, candidate_sha=candidate, upstream_in_candidate=contains_source)
    title = f'chore: integrate upstream {tag} [skip ci]'
    body = (f'Upstream: {UPSTREAM} {tag}\n\n'
            f'Pinned upstream commit: `{source}`\n\n'
            f'Custom base at preview: `{base}`\n\n'
            f'Merge preview: **{status}**.\n\n'
            f'Review branch: `{head}` at `{candidate}`; pinned upstream included: `{contains_source}`.\n\n'
            'Owner workflow policy is retained; application sources match the pinned upstream. '
            'An unmerged prepared candidate is never overwritten. An already merged candidate yields to the new tracking tip.\n\n'
            'This sync does not merge production, build, release, or deploy. '
            'Retain custom scheduling, session/cache ownership, shared-Key limits, '
            'regional video ownership, and account performance monitoring. '
            'Resolve semantic conflicts, run local regressions and migration/rollback '
            'checks, then publish a custom stable release. No CI was requested.\n')
    existing = gh_json('pr', 'list', '--repo', REPOSITORY, '--base', BASE,
                       '--head', head, '--state', 'open', '--json', 'number,url')
    if existing:
        github('PATCH', f'repos/{REPOSITORY}/pulls/{existing[0]["number"]}', {'title': title, 'body': body})
        result['pull_request'] = existing[0]['html_url']
    else:
        request = github('POST', f'repos/{REPOSITORY}/pulls', {'title': title, 'body': body, 'base': BASE, 'head': head})
        result['pull_request'] = request['html_url']
    if head == INTEGRATION:
        superseded = gh_json('pr', 'list', '--repo', REPOSITORY, '--base', BASE,
                             '--head', TRACKING, '--state', 'open', '--json', 'number,url')
        for request in superseded:
            github('PATCH', f"repos/{REPOSITORY}/pulls/{request['number']}",
                   {'state': 'closed', 'body': 'Superseded by the resolved integration review: ' + result['pull_request']})
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dry-run', action='store_true')
    parser.add_argument('--bootstrap-tracking', action='store_true',
                        help='one local owner-authorized transition to a tracking branch with owner workflows')
    args = parser.parse_args()
    print(json.dumps(sync(args.dry_run, args.bootstrap_tracking), ensure_ascii=False))
