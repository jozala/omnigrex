#!/usr/bin/env python3
"""Release designation, not a build system. GitHub access is only used in Actions."""
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import zipfile

REPO = 'jozala/omnigrex'
PRODUCTS = ('orchestrator', 'orchestrator-microsandbox', 'pi')
FILES = ('compose.pi.yaml', 'compose.pi-docker.yaml', 'compose.pi-microsandbox.yaml', '.env.pi.example')
REQUIRED = ('docker-check', 'development-qualification', 'microsandbox', 'check', 'publish', 'deploy-smoke', 'deploy-smoke-microsandbox', 'development-smoke-microsandbox')
ASSETS = ('payload.tar.gz', 'candidate.json', 'SHA256SUMS', 'release.json')


def require(ok, message):
    if not ok:
        raise ValueError(message)


def encode(value):
    return (json.dumps(value, sort_keys=True, indent=2) + '\n').encode()


def sha(data):
    return hashlib.sha256(data).hexdigest()


def api(path, method='GET', body=None, raw=False):
    # gh handles authenticated artifact redirects without exposing the token.
    cmd = ['gh', 'api', '-X', method, path]
    if raw:
        cmd += ['-H', 'Accept: application/octet-stream']
    if body is not None:
        cmd += ['--input', '-']
    data = subprocess.check_output(cmd, input=encode(body) if body is not None else None)
    return data if raw else json.loads(data)


def pages(path, key=None):
    result = []
    page = 1
    while True:
        data = api(path + ('&' if '?' in path else '?') + f'per_page=100&page={page}')
        batch = data[key] if key else data
        result.extend(batch)
        if len(batch) < 100:
            return result
        page += 1


def endpoint(path):
    return f'repos/{REPO}/{path}'


def identity():
    return dict(schema_version=1, repository=REPO, commit=os.environ['GITHUB_SHA'], run_id=int(os.environ['GITHUB_RUN_ID']), attempt=int(os.environ['GITHUB_RUN_ATTEMPT']), platform='linux/amd64')


def validate_manifest(m):
    require(m['schema_version'] == 1, 'unsupported candidate manifest schema')
    require(m['repository'] == REPO and re.fullmatch('[0-9a-f]{40}', m['commit']), 'invalid repository/commit')
    require(m['platform'] == 'linux/amd64', 'unsupported platform')
    require(type(m['run_id']) is int and m['run_id'] > 0 and type(m['attempt']) is int and m['attempt'] > 0, 'invalid run/attempt')
    require(set(m['images']) == set(PRODUCTS), 'product images must exclude development')
    for name, digest in m['images'].items():
        require(re.fullmatch(r'ghcr.io/jozala/omnigrex/' + name + r'@sha256:[0-9a-f]{64}', digest), 'invalid exact image digest')


def pack(out):
    out.mkdir()
    m = identity()
    m['images'] = {n: os.environ[n.upper().replace('-', '_') + '_IMAGE'] for n in PRODUCTS}
    validate_manifest(m)
    contents = {f'deploy/{f}': Path('deploy', f).read_bytes() for f in FILES}
    contents['build.json'] = encode(m)
    m['files'] = {n: sha(b) for n, b in contents.items()}
    with tarfile.open(out / 'payload.tar.gz', 'w:gz') as archive:
        for n, b in contents.items():
            info = tarfile.TarInfo(n)
            info.size = len(b)
            info.mode = 0o644
            archive.addfile(info, io.BytesIO(b))
    m['payload_sha256'] = sha((out / 'payload.tar.gz').read_bytes())
    (out / 'candidate.json').write_bytes(encode(m))
    checksums(out)
    verify(out)


def checksums(out):
    (out / 'SHA256SUMS').write_text(''.join(f'{sha((out / n).read_bytes())}  {n}\n' for n in ('payload.tar.gz', 'candidate.json')))


def verify(out):
    m = json.loads((out / 'candidate.json').read_bytes())
    validate_manifest(m)
    payload = (out / 'payload.tar.gz').read_bytes()
    require(sha(payload) == m['payload_sha256'], 'payload checksum mismatch')
    expected = {f'deploy/{f}' for f in FILES} | {'build.json'}
    require(set(m['files']) == expected, 'unexpected deployment files')
    with tarfile.open(fileobj=io.BytesIO(payload), mode='r:gz') as archive:
        members = archive.getmembers()
        require(len(members) == len(expected) and {x.name for x in members} == expected, 'unsafe archive paths or duplicate files')
        for x in members:
            require(x.isfile() and x.mode == 0o644 and x.size < 1024 * 1024, 'unsafe archive entry')
            b = archive.extractfile(x).read()
            require(sha(b) == m['files'][x.name], 'archive file checksum mismatch')
            if x.name == 'build.json':
                build = json.loads(b)
                require(all(build[k] == m[k] for k in ('repository', 'commit', 'run_id', 'attempt', 'platform', 'images')), 'build identity mismatch')
    expected_sums = ''.join(f'{sha((out / n).read_bytes())}  {n}\n' for n in ('payload.tar.gz', 'candidate.json'))
    require((out / 'SHA256SUMS').read_text() == expected_sums, 'asset checksums mismatch')
    return m


def jobs(run, attempt, names=REQUIRED):
    data = pages(endpoint(f'actions/runs/{run}/attempts/{attempt}/jobs'), 'jobs')
    evidence = {}
    for name in names:
        matches = [j for j in data if j['name'] == name]
        require(len(matches) == 1 and matches[0]['status'] == 'completed' and matches[0]['conclusion'] == 'success', f'required qualification unavailable or unsuccessful: {name}')
        evidence[name] = matches[0]['id']
    return evidence


def ready(out):
    m = verify(out)
    require(all(m[k] == v for k, v in identity().items()), 'candidate identity mismatch')
    m['qualification'] = jobs(m['run_id'], m['attempt'])
    (out / 'candidate.json').write_bytes(encode(m))
    checksums(out)


def version(value, alpha):
    pattern = r'0\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)' + (r'-alpha\.([1-9][0-9]*)' if alpha else '')
    match = re.fullmatch(pattern, value)
    require(match is not None, 'expected full pre-1.0 version without v prefix')
    result = tuple(int(x) for x in match.groups())
    require(result[0] >= 1, 'first supported target is 0.1.0')
    return result


def progression(value, alpha, breaking, releases, tags):
    v = version(value, alpha)
    require('v' + value not in tags and all(r['tag_name'] != 'v' + value for r in releases), 'version already exists (including draft/tag)')
    stable = []
    for r in releases:
        if not r['draft'] and not r['prerelease']:
            stable.append(version(r['tag_name'].removeprefix('v'), False))
    previous = max(stable, default=(0, 0))
    require(v[:2] > previous, 'stable target must move forward')
    if breaking and previous != (0, 0):
        require(v[0] > previous[0] and v[1] == 0, 'breaking changes require a minor bump from previous stable')
    if alpha:
        for r in releases:
            tag = r['tag_name'].removeprefix('v')
            if re.fullmatch(r'0\.[0-9]+\.[0-9]+-alpha\.[0-9]+', tag):
                old = version(tag, True)
                require(old[:2] != v[:2] or v[2] > old[2], 'alpha sequence must advance within target')


def download_artifact(run, attempt, out):
    name = f'release-candidate-{run}-{attempt}'
    artifacts = pages(endpoint(f'actions/runs/{run}/artifacts'), 'artifacts')
    found = [a for a in artifacts if a['name'] == name and not a['expired']]
    require(len(found) == 1, 'candidate artifact missing, expired, or ambiguous')
    blob = api(endpoint(f"actions/artifacts/{found[0]['id']}/zip"), raw=True)
    with zipfile.ZipFile(io.BytesIO(blob)) as z:
        require(set(z.namelist()) == set(ASSETS) - {'release.json'} and len(z.namelist()) == 3, 'invalid candidate artifact files')
        for n in z.namelist():
            (out / n).write_bytes(z.read(n))


def source(run, m):
    require(run['repository']['full_name'] == REPO and run['head_repository']['full_name'] == REPO, 'source repository mismatch')
    require(run['event'] == 'push' and run['head_branch'] == 'main' and run['path'] == '.github/workflows/ci.yaml', 'source must be main push CI')
    workflow = api(endpoint('actions/workflows/ci.yaml'))
    require(run['workflow_id'] == workflow['id'], 'source workflow mismatch')
    require(run['status'] == 'completed' and run['conclusion'] == 'success', 'source CI is not successfully complete')
    require((run['id'], run['run_attempt'], run['head_sha']) == (m['run_id'], m['attempt'], m['commit']), 'source commit/run/attempt mismatch')
    compare = api(endpoint(f"compare/{m['commit']}...main"))
    require(compare['status'] in ('ahead', 'identical'), 'source is not an ancestor of main')
    require(jobs(m['run_id'], m['attempt']) == m['qualification'], 'qualification job identity mismatch')
    jobs(m['run_id'], m['attempt'], ('release-candidate',))


def release_assets(release, out):
    require(not release['draft'] and release['prerelease'], 'selected alpha must be a published prerelease')
    for n in ASSETS:
        found = [a for a in release['assets'] if a['name'] == n]
        require(len(found) == 1, 'alpha release asset missing or duplicated: ' + n)
        (out / n).write_bytes(api(endpoint(f"releases/assets/{found[0]['id']}"), raw=True))


def publish(channel, out):
    require(os.environ['GITHUB_REPOSITORY'] == REPO and os.environ['GITHUB_REF'] == 'refs/heads/main' and os.environ['GITHUB_EVENT_NAME'] == 'workflow_dispatch', 'release must be dispatched on main')
    alpha = channel == 'alpha'
    value = os.environ['VERSION']
    version(value, alpha)
    releases = pages(endpoint('releases'))
    tags = {x['name'] for x in pages(endpoint('tags'))}
    notes = os.environ['NOTES'].strip()
    require(notes, 'release notes are required')
    out.mkdir()
    if alpha:
        breaking = os.environ['BREAKING'].strip()
        upgrades = os.environ['UPGRADES'].strip()
        require(breaking and upgrades, 'explicit breaking-change and upgrade declarations required (None is allowed)')
        run_id = os.environ['SOURCE_RUN']
        require(re.fullmatch('[1-9][0-9]*', run_id), 'source CI run ID required')
        run = api(endpoint(f'actions/runs/{run_id}'))
        download_artifact(run['id'], run['run_attempt'], out)
        m = verify(out)
        source(run, m)
        metadata = dict(version=value, channel=channel, breaking=breaking, upgrades=upgrades)
    else:
        selected = os.environ['ALPHA']
        version(selected, True)
        require(value == selected.split('-alpha.')[0], 'stable target must exactly remove selected alpha suffix')
        release = api(endpoint('releases/tags/v' + selected))
        release_assets(release, out)
        m = verify(out)
        require(set(m.get('qualification', {})) == set(REQUIRED) and all(type(i) is int and i > 0 for i in m['qualification'].values()), 'retained qualification evidence missing')
        metadata = json.loads((out / 'release.json').read_bytes())
        require(metadata['version'] == selected and metadata['channel'] == 'alpha' and metadata['commit'] == m['commit'] and metadata['payload_sha256'] == m['payload_sha256'], 'alpha release metadata mismatch')
        ref = api(endpoint('git/ref/tags/v' + selected))
        require(ref['object']['type'] == 'commit' and ref['object']['sha'] == m['commit'], 'alpha tag source mismatch')
        metadata.update(version=value, channel=channel, promoted_from=selected)
    progression(value, alpha, metadata['breaking'].lower() != 'none', releases, tags)
    metadata.update(commit=m['commit'], payload_sha256=m['payload_sha256'])
    (out / 'release.json').write_bytes(encode(metadata))
    body = notes + '\n\n## Breaking changes\n' + metadata['breaking'] + '\n\n## Upgrade requirements\n' + metadata['upgrades'] + f"\n\nSource: {m['commit']} (CI run {m['run_id']}, attempt {m['attempt']}).\nPre-1.0: recommended does not mean production-ready or permanently backward-compatible."
    # No --clobber, force update, build, or registry tag resolution is permitted.
    # Reserve an exact lightweight tag; GitHub rejects existing refs rather than moving them.
    api(endpoint('git/refs'), 'POST', dict(ref='refs/tags/v' + value, sha=m['commit']))
    draft = api(endpoint('releases'), 'POST', dict(tag_name='v' + value, target_commitish=m['commit'], name='v' + value, body=body, draft=True, prerelease=alpha))
    subprocess.run(['gh', 'release', 'upload', 'v' + value, '--repo', REPO] + [str(out / n) for n in ASSETS], check=True)
    attached = api(endpoint(f"releases/{draft['id']}"))
    require({a['name'] for a in attached['assets']} == set(ASSETS), 'draft asset set mismatch')
    for a in attached['assets']:
        data = api(endpoint(f"releases/assets/{a['id']}"), raw=True)
        require(data == (out / a['name']).read_bytes(), 'uploaded asset bytes differ')
    ref = api(endpoint('git/ref/tags/v' + value))
    require(ref['object']['type'] == 'commit' and ref['object']['sha'] == m['commit'], 'release tag source mismatch')
    api(endpoint(f"releases/{draft['id']}"), 'PATCH', dict(draft=False, make_latest='false' if alpha else 'true'))


if __name__ == '__main__':
    try:
        command, directory = sys.argv[1:]
        if command in ('alpha', 'stable'):
            publish(command, Path(directory))
        else:
            {'pack': pack, 'verify': verify, 'ready': ready}[command](Path(directory))
    except (ValueError, KeyError, OSError, tarfile.TarError, zipfile.BadZipFile, subprocess.CalledProcessError) as error:
        sys.exit(f'release failed: {error}')
