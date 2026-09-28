#!/usr/bin/env python3
"""Regression contract for #9154; optionally inspect another checkout."""
import pathlib
import re
import sys
import unittest

import yaml

ROOT = pathlib.Path(sys.argv.pop(1)) if len(sys.argv) > 1 else pathlib.Path(__file__).resolve().parents[2]


def workflow(name):
    return yaml.safe_load((ROOT / '.github/workflows' / name).read_text())


class V6ReleasePath(unittest.TestCase):
    def test_release_uses_v6_at_every_boundary(self):
        release = workflow('tagged-release.yml')
        self.assertEqual(release['concurrency']['group'], 'tagged-release-v6')
        self.assertIn("head_branch == 'v6'", release['jobs']['decide']['if'])
        steps = [s for job in release['jobs'].values() for s in job.get('steps', [])]
        derive = next(s for s in steps if s.get('id') == 'derive')
        self.assertEqual(derive['env']['RELEASE_LINE'], 'v6')
        scripts = '\n'.join(s.get('run', '') for s in steps)
        for required in ('/branches/v6', '--base v6', 'docker.yml --ref v6',
                         'git fetch origin v6', '"$release_sha" origin/v6', '--target v6'):
            self.assertIn(required, scripts)
        # No executable old-line pin may survive, including a second backstop.
        executable = '\n'.join(line for line in scripts.splitlines()
                               if not line.lstrip().startswith('#'))
        self.assertIsNone(re.search(r'\bv5\b', executable))

    def test_promotion_checks_v6(self):
        promotion = workflow('promote-stable.yml')
        self.assertEqual(promotion['env']['RELEASE_BRANCH'], 'v6')
        self.assertEqual(promotion['concurrency']['group'], 'stable-promotion-v6')
        checkout = promotion['jobs']['promote']['steps'][0]
        self.assertEqual(checkout['with']['ref'], 'v6')

    def test_all_images_and_mirrors_use_branch_gated_channels(self):
        docker = workflow('docker.yml')
        publishers, mirrors = [], []
        for job in docker['jobs'].values():
            for step in job.get('steps', []):
                script = step.get('run', '')
                if 'src/scripts/publish-image-tags.sh' in script:
                    publishers.append(script)
                    self.assertRegex(script, r'v6 false candidate,latest,edge\s*$')
                env = step.get('env', {})
                if 'CHANNELS' in env:
                    mirrors.append(env)
                    self.assertEqual(env['RELEASE_BRANCH'], 'v6')
                    self.assertEqual(env['INCLUDE_LATEST'], 'false')
                    self.assertEqual(env['CHANNELS'], 'candidate,latest,edge')
        self.assertEqual(len(publishers), 3)
        self.assertEqual(len(mirrors), 3)

    def test_v6_is_a_release_line(self):
        manifest = yaml.safe_load((ROOT / '.github/release-lines.yml').read_text())
        self.assertIn('v6', manifest['release_lines'])
        monitor = workflow('dco-post-merge.yml')
        self.assertIn('v6', next(job['strategy']['matrix']['branch']
                                for job in monitor['jobs'].values() if 'strategy' in job))


if __name__ == '__main__':
    unittest.main()
