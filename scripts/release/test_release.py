import copy
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import release as r


class ReleaseTest(unittest.TestCase):
    def setUp(self):
        previous = Path.cwd()
        os.chdir(Path(__file__).resolve().parents[2])
        self.addCleanup(os.chdir, previous)
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.out = Path(self.temp.name) / 'candidate'
        self.env = dict(GITHUB_SHA='a' * 40, GITHUB_RUN_ID='17', GITHUB_RUN_ATTEMPT='2', GITHUB_REPOSITORY=r.REPO, GITHUB_REF='refs/heads/main', GITHUB_EVENT_NAME='workflow_dispatch', VERSION='0.1.0-alpha.1', SOURCE_RUN='17', NOTES='Human notes', BREAKING='None', UPGRADES='Fresh install; historical upgrades unqualified')
        self.env.update({n.upper().replace('-', '_') + '_IMAGE': f'ghcr.io/jozala/omnigrex/{n}@sha256:' + str(i) * 64 for i, n in enumerate(r.PRODUCTS)})
        self.enterContext(patch.dict(os.environ, self.env))
        r.pack(self.out)
        self.m = r.verify(self.out)
        self.m['qualification'] = {n: i + 1 for i, n in enumerate(r.REQUIRED)}
        (self.out / 'candidate.json').write_bytes(r.encode(self.m))
        r.checksums(self.out)
        self.run = dict(id=17, run_attempt=2, head_sha='a' * 40, repository={'full_name': r.REPO}, head_repository={'full_name': r.REPO}, event='push', head_branch='main', path='.github/workflows/ci.yaml', workflow_id=12, status='completed', conclusion='success')

    def test_payload_and_no_checkout_compose(self):
        r.verify(self.out)
        extracted = Path(self.temp.name) / 'extracted'
        with tarfile.open(self.out / 'payload.tar.gz') as archive:
            archive.extractall(extracted)  # verify() has rejected links, duplicates and unsafe paths
        build = json.loads((extracted / 'build.json').read_bytes())
        self.assertEqual(build['images'], self.m['images'])
        self.assertEqual(set(p.name for p in (extracted / 'deploy').iterdir()), set(r.FILES))
        env = dict(os.environ, OMNIGREX_ORCHESTRATOR_IMAGE=build['images']['orchestrator'], OMNIGREX_PI_IMAGE=build['images']['pi'], OMNIGREX_PI_PLATFORM='linux/amd64', OMNIGREX_DOCKER_GID='0', OMNIGREX_SECRET_GID='1', OMNIGREX_MICROSANDBOX_KVM_GID='2', OMNIGREX_GITHUB_DEVELOPER_APP_ID='1', OMNIGREX_GITHUB_REVIEWER_APP_ID='2')
        for key in ('DATABASE_PASSWORD', 'GITHUB_DEVELOPER_PRIVATE_KEY', 'GITHUB_REVIEWER_PRIVATE_KEY', 'GITHUB_WEBHOOK_SECRET', 'PROVIDER_CREDENTIALS'):
            env['OMNIGREX_' + key + '_FILE'] = '/dev/null'
        for backend in ('docker', 'microsandbox'):
            env['OMNIGREX_ORCHESTRATOR_IMAGE'] = build['images']['orchestrator' if backend == 'docker' else 'orchestrator-microsandbox']
            subprocess.run(['docker', 'compose', '-f', 'compose.pi.yaml', '-f', f'compose.pi-{backend}.yaml', 'config', '--quiet'], cwd=extracted / 'deploy', env=env, check=True)

    def test_tampering(self):
        for file in ('payload.tar.gz', 'SHA256SUMS', 'candidate.json'):
            with self.subTest(file=file):
                p = self.out / file
                original = p.read_bytes()
                p.write_bytes(original + b'bad')
                with self.assertRaises((ValueError, json.JSONDecodeError)):
                    r.verify(self.out)
                p.write_bytes(original)
        # Even a self-consistent checksum cannot authorize traversal or secrets.
        for name, kind in [('../escape', tarfile.REGTYPE), ('deploy/secrets/key', tarfile.REGTYPE), ('deploy/compose.pi.yaml', tarfile.SYMTYPE)]:
            with self.subTest(name=name):
                with tarfile.open(self.out / 'payload.tar.gz', 'w:gz') as archive:
                    info = tarfile.TarInfo(name)
                    info.type = kind
                    archive.addfile(info, io.BytesIO())
                m = copy.deepcopy(self.m)
                m['payload_sha256'] = r.sha((self.out / 'payload.tar.gz').read_bytes())
                (self.out / 'candidate.json').write_bytes(r.encode(m))
                r.checksums(self.out)
                with self.assertRaisesRegex(ValueError, 'archive'):
                    r.verify(self.out)

    def test_manifest_build_digest_and_platform_disagreement(self):
        for key, value in [('images', dict(self.m['images'], pi='ghcr.io/jozala/omnigrex/pi@sha256:' + 'f' * 64)), ('commit', 'b' * 40), ('run_id', 18), ('attempt', 3), ('platform', 'linux/arm64')]:
            with self.subTest(key=key):
                changed = dict(self.m, **{key: value})
                (self.out / 'candidate.json').write_bytes(r.encode(changed))
                r.checksums(self.out)
                with self.assertRaises(ValueError):
                    r.verify(self.out)

    def test_candidate_readiness_binds_exact_attempt_after_all_jobs(self):
        original_payload = (self.out / 'payload.tar.gz').read_bytes()
        with patch.object(r, 'api', self.source_api):
            r.ready(self.out)
            self.assertEqual(r.verify(self.out)['qualification'], self.m['qualification'])
            self.assertEqual((self.out / 'payload.tar.gz').read_bytes(), original_payload)
            with patch.dict(os.environ, GITHUB_RUN_ATTEMPT='3'), self.assertRaisesRegex(ValueError, 'identity mismatch'):
                r.ready(self.out)
        with patch.object(r, 'api', return_value={'jobs': []}), self.assertRaisesRegex(ValueError, 'required qualification'):
            r.ready(self.out)

    def test_versions(self):
        for value in ('1.0.0', '0.0.0', 'v0.1.0', '0.01.0', '0.1.0-beta.1', '0.1.0-alpha.0', '0.1.0-alpha.01', '0.1.0-alpha.1\n'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                r.version(value, True)
        stable = [dict(tag_name='v0.1.0', draft=False, prerelease=False)]
        r.progression('0.1.1-alpha.1', True, False, stable, set())
        r.progression('0.2.0-alpha.3', True, True, stable, set())
        for value, alpha, breaking, tags in [('0.1.0', False, False, set()), ('0.1.1-alpha.1', True, True, set()), ('0.1.1-alpha.1', True, False, {'v0.1.1-alpha.1'}), ('0.0.9', False, False, set())]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                r.progression(value, alpha, breaking, stable, tags)
        with self.assertRaisesRegex(ValueError, 'move forward'):
            r.progression('0.2.5', False, False, [dict(tag_name='v0.3.0', draft=False, prerelease=False)], set())
        old = stable + [dict(tag_name='v0.2.0-alpha.3', draft=False, prerelease=True)]
        with self.assertRaisesRegex(ValueError, 'sequence'):
            r.progression('0.2.0-alpha.2', True, False, old, set())
        r.progression('0.2.0-alpha.4', True, True, old, set())

    def source_api(self, path, *args, **kwargs):
        if path.endswith('actions/workflows/ci.yaml'):
            return {'id': 12}
        if 'compare/' in path:
            return {'status': 'ahead'}
        if '/jobs?' in path:
            return {'jobs': [dict(name=n, id=i, status='completed', conclusion='success') for n, i in dict(self.m['qualification'], **{'release-candidate': 99}).items()]}
        raise AssertionError(path)

    def test_source_provenance(self):
        with patch.object(r, 'api', self.source_api):
            r.source(self.run, self.m)
            for key, value in [('event', 'pull_request'), ('head_branch', 'other'), ('head_repository', {'full_name': 'fork/omnigrex'}), ('repository', {'full_name': 'fork/omnigrex'}), ('path', '.github/workflows/other.yaml'), ('workflow_id', 99), ('conclusion', 'failure'), ('status', 'in_progress'), ('head_sha', 'b' * 40), ('run_attempt', 1), ('id', 18)]:
                with self.subTest(key=key), self.assertRaises(ValueError):
                    r.source(dict(self.run, **{key: value}), self.m)
            m = copy.deepcopy(self.m)
            m['qualification']['check'] = 999
            with self.assertRaisesRegex(ValueError, 'job identity'):
                r.source(self.run, m)

    def test_source_must_remain_on_main(self):
        def api(path, *args, **kwargs):
            if 'compare/' in path:
                return {'status': 'diverged'}
            return self.source_api(path, *args, **kwargs)
        with patch.object(r, 'api', api), self.assertRaisesRegex(ValueError, 'ancestor'):
            r.source(self.run, self.m)

    def test_all_required_evidence_fails_closed(self):
        for name in r.REQUIRED:
            for conclusion in ('failure', 'cancelled', 'skipped', None, 'pending', 'missing'):
                data = [dict(name=n, id=i, status='completed', conclusion='success') for n, i in self.m['qualification'].items()]
                data = [j for j in data if j['name'] != name or conclusion != 'missing']
                for j in data:
                    if j['name'] == name:
                        j['conclusion'] = conclusion
                with self.subTest(name=name, conclusion=conclusion), patch.object(r, 'api', return_value={'jobs': data}), self.assertRaises(ValueError):
                    r.jobs(17, 2)

    def test_expired_and_malformed_artifact(self):
        for artifacts in ([], [dict(name='release-candidate-17-2', expired=True)]):
            with patch.object(r, 'api', return_value={'artifacts': artifacts}), self.assertRaisesRegex(ValueError, 'missing, expired'):
                r.download_artifact(17, 2, self.out)
        b = io.BytesIO()
        with zipfile.ZipFile(b, 'w') as z:
            z.writestr('../secret', b'no')
        def api(path, **kwargs):
            if kwargs.get('raw'):
                return b.getvalue()
            return {'artifacts': [dict(name='release-candidate-17-2', expired=False, id=8)]}
        with patch.object(r, 'api', api), self.assertRaisesRegex(ValueError, 'artifact files'):
            r.download_artifact(17, 2, self.out)

    def test_publication_redesignation_and_promotion_without_ci_artifacts(self):
        releases = []
        assets = {}
        drafts = {}
        tags = {}
        candidate = {n: (self.out / n).read_bytes() for n in r.ASSETS if n != 'release.json'}
        requests = []
        fail_upload = [False]
        corrupt_upload = [False]
        def api(path, method='GET', body=None, raw=False):
            requests.append(path)
            tail = path.split(r.REPO + '/')[1]
            if tail.startswith('releases?'):
                return copy.deepcopy(releases)
            if tail.startswith('tags?'):
                return [{'name': n} for n in tags]
            if tail == 'actions/runs/17':
                return self.run
            if tail == 'git/refs' and method == 'POST':
                name = body['ref'].removeprefix('refs/tags/')
                self.assertNotIn(name, tags)
                tags[name] = body['sha']
                return {}
            if tail == 'releases' and method == 'POST':
                new = dict(body, id=len(drafts) + 1, assets=[])
                drafts[new['id']] = new
                return new
            if tail.startswith('releases/assets/'):
                return assets[int(tail.split('/')[-1])] + (b'corrupt' if corrupt_upload[0] else b'')
            if tail.startswith('releases/tags/'):
                return next(x for x in releases if x['tag_name'] == tail.split('/')[-1])
            if tail.startswith('git/ref/tags/'):
                return {'object': {'type': 'commit', 'sha': tags[tail.split('/')[-1]]}}
            if re_match := __import__('re').fullmatch(r'releases/([0-9]+)', tail):
                draft = drafts[int(re_match[1])]
                if method == 'PATCH':
                    draft.update(body)
                    tags[draft['tag_name']] = draft['target_commitish']
                    releases.append(copy.deepcopy(draft))
                return draft
            return self.source_api(path)
        def artifact(run, attempt, out):
            for n, b in candidate.items():
                (out / n).write_bytes(b)
        def upload(cmd, **kwargs):
            self.assertEqual(cmd[:3], ['gh', 'release', 'upload'])
            if fail_upload[0]:
                raise subprocess.CalledProcessError(1, cmd)
            draft = drafts[len(drafts)]
            for file in cmd[6:]:
                p = Path(file)
                i = len(assets) + 1
                assets[i] = p.read_bytes()
                draft['assets'].append(dict(id=i, name=p.name))
        with patch.object(r, 'api', api), patch.object(r, 'download_artifact', artifact), patch.object(r.subprocess, 'run', upload):
            for failure in ('upload', 'verification'):
                fail_upload[0] = failure == 'upload'
                corrupt_upload[0] = failure == 'verification'
                with self.assertRaises((ValueError, subprocess.CalledProcessError)):
                    r.publish('alpha', Path(self.temp.name) / ('failed-' + failure))
                self.assertEqual(releases, [])
                self.assertTrue(drafts[1]['draft'])
                # Documented manual cleanup, not a custom automated resume path.
                drafts.clear()
                tags.clear()
                assets.clear()
            fail_upload[0] = corrupt_upload[0] = False
            first = Path(self.temp.name) / 'alpha'
            r.publish('alpha', first)
            original = copy.deepcopy(releases[0])
            os.environ.update(VERSION='0.2.0-alpha.1', BREAKING='New storage format')
            r.publish('alpha', Path(self.temp.name) / 'redesignated')
            self.assertEqual(releases[0], original)
            self.assertEqual((first / 'payload.tar.gz').read_bytes(), candidate['payload.tar.gz'])
            with self.assertRaisesRegex(ValueError, 'already exists'):
                r.publish('alpha', Path(self.temp.name) / 'duplicate')
            os.environ.update(VERSION='0.3.0', ALPHA='0.2.0-alpha.1')
            with self.assertRaisesRegex(ValueError, 'exactly remove'):
                r.publish('stable', Path(self.temp.name) / 'wrong')
            requests.clear()
            os.environ['VERSION'] = '0.2.0'
            with patch.object(r, 'download_artifact', side_effect=AssertionError('CI artifact expired')):
                stable = Path(self.temp.name) / 'stable'
                r.publish('stable', stable)
            self.assertFalse(any('actions/' in x for x in requests))
            for name, data in candidate.items():
                self.assertEqual((stable / name).read_bytes(), data)
            self.assertEqual(r.verify(stable)['images'], self.m['images'])
            self.assertFalse(releases[-1]['prerelease'])
            self.assertEqual(releases[-1]['make_latest'], 'true')
            self.assertEqual(releases[0]['make_latest'], 'false')
            self.assertEqual(tags['v0.2.0'], tags['v0.2.0-alpha.1'])


if __name__ == '__main__':
    unittest.main()
