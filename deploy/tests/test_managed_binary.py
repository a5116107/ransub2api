import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ENTRYPOINT = Path(__file__).resolve().parents[1] / 'docker-entrypoint.sh'


class ManagedBinaryTest(unittest.TestCase):
    def test_online_update_survives_recreation_and_new_image_is_explicit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            tools = root / 'tools'
            tools.mkdir()
            identity = tools / 'id'
            identity.write_text('#!/bin/sh\necho 1000\n')
            identity.chmod(0o755)
            source = root / 'sub2api'
            managed = root / 'managed'
            managed.mkdir()
            env = {**os.environ, 'PATH': str(tools) + os.pathsep + os.environ['PATH'], 'SUB2API_BINARY_DIR': str(managed)}

            def program(path, text):
                path.write_text(f'#!/bin/sh\necho {text} "$@"\n')
                path.chmod(0o755)

            def run():
                return subprocess.check_output(['sh', str(ENTRYPOINT), str(source), '--version'], text=True, env=env).strip()

            program(source, 'image-one')
            self.assertEqual(run(), 'image-one --version')
            program(managed / 'sub2api', 'online-update')
            self.assertEqual(run(), 'online-update --version')
            self.assertEqual(run(), 'online-update --version')
            program(source, 'image-two')
            self.assertEqual(run(), 'image-two --version')
            self.assertIn('online-update', (managed / 'sub2api.backup').read_text())
            self.assertFalse(list(managed.glob('.sub2api-image-*')))

    def test_first_adoption_preserves_existing_managed_binary(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            tools = root / 'tools'
            tools.mkdir()
            (tools / 'id').write_text('#!/bin/sh\necho 1000\n')
            (tools / 'id').chmod(0o755)
            source = root / 'sub2api'
            source.write_text('#!/bin/sh\necho image\n')
            source.chmod(0o755)
            managed = root / 'managed'
            managed.mkdir()
            (managed / 'sub2api').write_text('#!/bin/sh\necho retained\n')
            (managed / 'sub2api').chmod(0o755)
            env = {**os.environ, 'PATH': str(tools) + os.pathsep + os.environ['PATH'], 'SUB2API_BINARY_DIR': str(managed)}
            output = subprocess.check_output(['sh', str(ENTRYPOINT), str(source)], text=True, env=env)
            self.assertEqual(output.strip(), 'retained')


if __name__ == '__main__':
    unittest.main()
