#!/usr/bin/env python3
"""Exercise setup effects and refusal paths without Docker Desktop or network."""
import argparse
import importlib.util
import io
import json
import os
import runpy
import pathlib
import subprocess
import tempfile
import unittest
import sys
sys.dont_write_bytecode = True
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('bootstrap', pathlib.Path(__file__).with_name('bootstrap.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

class SetupTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.config = {'serverUrl': 'https://intake.invalid', 'serverTokenFile': '/state/private/pc-token', 'workerId':'mac-worker'}
        (self.root/'k2').write_text(json.dumps(self.config))
        args = dict(config=str(self.root/'k2'), compose=str(self.root/'compose'), root=str(self.root),
                    overlay=str(self.root/'overlay'), flavor='')
        for key in ['pc_token_file','gitea_token_file','host_image_token_file','codex_auth_file','claude_auth_file']:
            p=self.root/key
            p.write_text('idh_'+'a'*43 if key=='host_image_token_file' else key+'-secret')
            p.chmod(0o600)
            args[key]=str(p)
        self.args=argparse.Namespace(**args)
        self.volumes={}
        self.calls=[]
        self.checked=False
        self.started=False
        self.fail_check=False
        self.running=False
        self.context='desktop-linux'
        self.endpoint='unix:///fixture/docker.sock'
        self.info_failures=0
        self.context_missing=0
        self.desired={'tag':'a'*12,'revision':'a'*40,'digest':'sha256:'+'b'*64,'flavor':'','registry':'registry.invalid'}
        self.requests=[]

    def docker(self, cmd, **kwargs):
        self.calls.append(cmd)
        if cmd[:3]==['docker','--context','desktop-linux']:
            cmd=['docker']+cmd[3:]
        code=0
        output=''
        if cmd[:2]==['open','-a']:
            pass
        elif cmd[1]=='info':
            if self.info_failures:
                self.info_failures-=1
                code=1
        elif cmd[1]=='context':
            output=self.context if cmd[2]=='show' else self.endpoint
            if cmd[2]=='inspect' and self.context_missing:
                self.context_missing-=1
                code=1
        elif cmd[1:3]==['volume','inspect']:
            if cmd[3] in self.volumes:
                output=json.dumps([{'Labels':self.volumes[cmd[3]]}])
            else: code=1
        elif cmd[1:3]==['volume','create']:
            name=cmd[-1]
            self.volumes[name]={'issue-desk.bootstrap':cmd[-2].split('=',1)[1]}
            (self.root/name).mkdir()
        elif cmd[1]=='ps':
            output='existing' if self.running else ''
        elif cmd[1]=='pull':
            self.assertIn('linux/amd64',cmd)
            self.assertTrue(cmd[-1].endswith('@'+self.desired['digest']))
        elif cmd[1]=='run':
            mount=cmd[cmd.index('--mount')+1]
            volume=mount.split('source=')[1].split(',')[0]
            target=mount.split('target=')[1]
            path=self.root/volume/cmd[-1].removeprefix(target+'/')
            # Execute the shipped Node file-placement program, including the
            # exclusive-write/unchanged-input contract, against private fixtures.
            proc=self.real_run(['node','-e',cmd[-2],str(path)],input=kwargs['input'],capture_output=True)
            code=proc.returncode
        elif cmd[1]=='compose':
            overlay=json.loads(pathlib.Path(self.args.overlay).read_text())
            self.assertIn(kwargs['env']['CODEX_AUTH_DIR'],overlay['volumes'])
            if 'check' in cmd:
                self.checked=True
                code=1 if self.fail_check else 0
            elif 'up' in cmd:
                self.assertTrue(self.checked)
                self.started=True
        else:
            self.fail(str(cmd))
        if code and kwargs.get('check'):
            raise subprocess.CalledProcessError(code,cmd)
        return subprocess.CompletedProcess(cmd,code,stdout=output,stderr='')

    def opener(self, *args):
        test=self
        class Opener:
            def open(self, req, timeout):
                test.requests.append(req)
                return io.BytesIO(json.dumps({'desired':test.desired}).encode())
        return Opener()

    def setup(self, cli=False):
        self.real_run=subprocess.run
        with patch.object(module.subprocess,'run',side_effect=self.docker), patch.object(module.urllib.request,'build_opener',side_effect=self.opener), patch.object(module.time,'sleep'):
            if cli:
                argv = ['bootstrap.py']
                for key, value in vars(self.args).items():
                    if key != 'flavor':
                        argv.extend(['--' + key.replace('_', '-'), value])
                with patch.object(sys, 'argv', argv):
                    runpy.run_path(str(pathlib.Path(__file__).with_name('bootstrap.py')), run_name='__main__')
            else:
                module.bootstrap(self.args)

    def test_new_setup_fetches_correct_route_places_secrets_then_checks_and_starts(self):
        self.info_failures=1
        self.context_missing=1
        self.setup()
        self.assertTrue(self.started)
        req=self.requests[0]
        self.assertEqual(req.full_url,'https://intake.invalid/api/v1/hosts/mac-worker/desired-image?flavor=&currentRevision=')
        self.assertEqual(req.get_header('Authorization'),'Bearer idh_'+'a'*43)
        self.assertEqual(len(self.volumes),4)
        state=self.root/'issue-desk-mac-worker-state'
        self.assertEqual(json.loads((state/'config.local.json').read_text()),self.config)
        self.assertEqual((state/'private/pc-token').read_text(),'pc_token_file-secret')
        self.assertEqual((state/'private/worker-token').stat().st_mode & 0o777,0o600)
        overlay=json.loads((self.root/'overlay').read_text())
        self.assertEqual(overlay['services']['issue-desk-client']['platform'],'linux/amd64')
        self.assertIn(['open','-a','Docker'],self.calls)
        self.assertNotIn('-secret',repr(self.calls))

    def test_default_command_fetches_python_distribution(self):
        self.desired.update(flavor='-python', tag='a'*12+'-client-python')
        self.setup(cli=True)
        self.assertTrue(self.started)
        self.assertIn('flavor=-python&', self.requests[0].full_url)
        pull = next(cmd for cmd in self.calls if 'pull' in cmd)
        self.assertEqual(pull[-1], 'registry.invalid/issue-desk-client-python@'+self.desired['digest'])

    def test_unavailable_malformed_or_mismatched_distribution_stops_safely(self):
        valid = dict(self.desired)
        for desired in [None, [], 'private-response-value', 42, {},
                        dict(valid, revision=None), dict(valid, digest=[]),
                        dict(valid, flavor='-python', tag='a'*12+'-client-python')]:
            with self.subTest(desired=desired):
                self.desired = desired
                with self.assertRaises(SystemExit) as stopped:
                    self.setup()
                diagnostic = str(stopped.exception)
                self.assertTrue(diagnostic)
                self.assertNotIn('private-response-value', diagnostic)
                self.assertNotIn('idh_'+'a'*43, diagnostic)
                self.assertFalse(self.calls)
                self.assertFalse((self.root/'docker-bootstrap.json').exists())

    def test_foreign_volume_and_running_worker_are_never_adopted(self):
        for scenario in ['volume','container','context','remote-endpoint']:
            with self.subTest(scenario=scenario):
                self.volumes={}
                self.running=scenario=='container'
                self.context='remote' if scenario=='context' else 'desktop-linux'
                self.endpoint='ssh://nas' if scenario=='remote-endpoint' else 'unix:///fixture/docker.sock'
                if scenario=='volume': self.volumes['issue-desk-mac-worker-codex']={}
                with self.assertRaises(SystemExit): self.setup()
                self.assertFalse(self.started)
                self.assertFalse((self.root/'docker-bootstrap.json').exists())

    def test_failed_check_can_resume_but_cannot_replace_existing_secret(self):
        self.fail_check=True
        with self.assertRaises(subprocess.CalledProcessError): self.setup()
        self.assertFalse(self.started)
        self.fail_check=False
        pathlib.Path(self.args.pc_token_file).write_text('replacement')
        with self.assertRaises(subprocess.CalledProcessError): self.setup()
        self.assertFalse(self.started)
        pathlib.Path(self.args.pc_token_file).write_text('pc_token_file-secret')
        self.setup()
        self.assertTrue(self.started)

    def test_invalid_distribution_and_unprotected_secret_stop_before_docker(self):
        self.desired['tag']='old'
        with self.assertRaises(SystemExit): self.setup()
        self.assertFalse(self.calls)
        self.desired['tag']='a'*12
        pathlib.Path(self.args.pc_token_file).chmod(0o644)
        with self.assertRaises(SystemExit): self.setup()
        self.assertFalse(self.calls)

if __name__=='__main__':
    unittest.main()
