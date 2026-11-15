#!/usr/bin/env python3
"""Native CPA provider-subscription guard e2e. Dummy credentials and loopback upstreams only.

Proves, with a real CPA binary and the native tea-quota-router + tea-combos plugins:
  * a dummy Claude OAuth file and a synthetic Claude API key serve the same model;
  * OAuth exhausted in the snapshot  -> ordinary route rejects, no paid mock request;
  * OAuth disabled in core while the snapshot still lists it enabled -> ordinary route
    rejects, no paid escape;
  * an explicit account-pinned API combo stays allowed;
  * an API-only model route stays allowed.
The dummy OAuth token must never leave the box: the OAuth file's proxy_url and the process
proxy env point at a loopback tripwire that records and refuses every connection, and the
mock upstream flags any request that carries the dummy token.
"""
import datetime, http.server, json, os, pathlib, shutil, socket, socketserver, subprocess, tempfile, threading, time, urllib.error, urllib.request
import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
UTC = datetime.timezone.utc
OAUTH_TOKEN = 'dummy-oauth-access-token-not-real'
API_KEY = 'synthetic-claude-api-key'
CLIENT = 'synthetic-client'
MGMT = 'synthetic-management'
API_ONLY = 'synthetic-api-only-model'
PIN_COMBO = 'pinned-api-combo'
AUTH_FILE = 'claude-dummy@example.test.json'


class Mock(http.server.BaseHTTPRequestHandler):
    """Loopback Anthropic Messages upstream. Records which credential header arrived."""
    calls = []
    lock = threading.Lock()

    def log_message(self, *a):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get('Content-Length') or 0)) or b'{}')
        hdrs = ' '.join(str(v) for v in self.headers.values())
        lane = 'oauth' if OAUTH_TOKEN in hdrs else ('api' if API_KEY in hdrs else 'unknown')
        with self.lock:
            self.calls.append({'lane': lane, 'path': self.path.split('?')[0], 'model': body.get('model'), 'stream': bool(body.get('stream')), 'oauthTokenSeen': OAUTH_TOKEN in hdrs})
        model = body.get('model')
        usage = {'input_tokens': 3, 'output_tokens': 1}
        if body.get('stream'):
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            ev = [('message_start', {'type': 'message_start', 'message': {'id': 'msg_synthetic', 'type': 'message', 'role': 'assistant', 'model': model, 'content': [], 'stop_reason': None, 'stop_sequence': None, 'usage': {'input_tokens': 3, 'output_tokens': 0}}}),
                  ('content_block_start', {'type': 'content_block_start', 'index': 0, 'content_block': {'type': 'text', 'text': ''}}),
                  ('content_block_delta', {'type': 'content_block_delta', 'index': 0, 'delta': {'type': 'text_delta', 'text': lane + '-lane'}}),
                  ('content_block_stop', {'type': 'content_block_stop', 'index': 0}),
                  ('message_delta', {'type': 'message_delta', 'delta': {'stop_reason': 'end_turn', 'stop_sequence': None}, 'usage': {'output_tokens': 1}}),
                  ('message_stop', {'type': 'message_stop'})]
            for name, data in ev:
                self.wfile.write(('event: %s\ndata: %s\n\n' % (name, json.dumps(data))).encode())
            self.wfile.flush()
            return
        raw = json.dumps({'id': 'msg_synthetic', 'type': 'message', 'role': 'assistant', 'model': model, 'content': [{'type': 'text', 'text': lane + '-lane'}], 'stop_reason': 'end_turn', 'stop_sequence': None, 'usage': usage}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


class Tripwire(socketserver.ThreadingMixIn, socketserver.TCPServer):
    """Loopback 'proxy' that records every attempted outbound request and answers 502."""
    allow_reuse_address = True
    daemon_threads = True
    hits = []
    lock = threading.Lock()


class TripHandler(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(2)
        data = b''
        try:
            while b'\r\n\r\n' not in data and len(data) < 65536:
                chunk = self.request.recv(4096)
                if not chunk:
                    break
                data += chunk
        except OSError:
            pass
        line = data.split(b'\r\n', 1)[0].decode('latin1', 'replace')[:200]
        with Tripwire.lock:
            Tripwire.hits.append({'requestLine': line, 'oauthTokenSeen': OAUTH_TOKEN.encode() in data})
        try:
            self.request.sendall(b'HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n')
        except OSError:
            pass


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def main():
    checks = {}
    report = {'syntheticOnly': True, 'checks': checks}
    proc = log = mock = trip = None
    run = pathlib.Path(tempfile.mkdtemp(prefix='quota-subscription-'))
    (run / 'plugins').mkdir()
    (run / 'auth').mkdir()
    env = dict(os.environ, GOTOOLCHAIN='auto', GOMAXPROCS='2', CGO_ENABLED='1')

    def check(name, ok, detail=None):
        checks[name] = bool(ok)
        if not ok:
            raise AssertionError('%s: %s' % (name, str(detail)[:600]))

    try:
        cpa = os.environ.get('CPA_BINARY')
        if not cpa or not os.access(cpa, os.X_OK):
            raise RuntimeError('CPA_BINARY must point to an executable cli-proxy-api')
        for name in ['tea-quota-router', 'tea-combos']:
            subprocess.run(['go', 'build', '-p=2', '-buildmode=c-shared', '-o', str(run / 'plugins' / (name + '.so')), '.'], cwd=ROOT / 'plugins' / name, env=env, check=True)
        for h in (run / 'plugins').glob('*.h'):
            h.unlink()
        mock = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Mock)
        threading.Thread(target=mock.serve_forever, daemon=True).start()
        trip = Tripwire(('127.0.0.1', 0), TripHandler)
        threading.Thread(target=trip.serve_forever, daemon=True).start()
        proxy = 'http://127.0.0.1:%d' % trip.server_address[1]
        env.update(HTTP_PROXY=proxy, HTTPS_PROXY=proxy, ALL_PROXY=proxy, http_proxy=proxy, https_proxy=proxy, all_proxy=proxy, NO_PROXY='127.0.0.1,localhost', no_proxy='127.0.0.1,localhost')
        port = free_port()
        base = 'http://127.0.0.1:%d' % port
        prefix = '/v0/management/plugins/tea-quota-router/'
        auth_path = run / 'auth' / AUTH_FILE

        def write_auth(disabled=False):
            auth_path.write_text(json.dumps({'type': 'claude', 'access_token': OAUTH_TOKEN, 'email': 'dummy@example.test', 'expired': '2099-01-01T00:00:00Z', 'disabled': disabled, 'proxy_url': proxy}))

        qcfg = {'enabled': True, 'dataDir': str(run / 'quota'), 'schedulerMode': 'off', 'pollIntervalSeconds': 180}
        cfg = {'config-version': 8, 'server': {'host': '127.0.0.1', 'port': port}, 'auth-dir': str(run / 'auth'), 'access': {'api-keys': [CLIENT]},
               'management': {'secret-key': MGMT, 'allow-remote': False, 'disable-control-panel': True},
               'routing': {'strategy': 'round-robin', 'session-affinity': False, 'retry': {'request-retry': 0, 'max-retry-credentials': 1, 'max-retry-interval': 0}},
               'plugins': {'enabled': True, 'dir': str(run / 'plugins'), 'configs': {'tea-quota-router': qcfg,
                           'tea-combos': {'enabled': True, 'statePath': str(run / 'combos.json'), 'quotaSnapshotPath': str(run / 'quota/snapshot-v1.json'), 'quotaMaxAgeSeconds': 600}}}}

        def call(path, method='GET', payload=None, headers=None):
            h = {'Authorization': 'Bearer ' + MGMT, 'Content-Type': 'application/json'}
            h.update(headers or {})
            q = urllib.request.Request(base + path, method=method, data=json.dumps(payload).encode() if payload is not None else None, headers=h)
            try:
                with urllib.request.urlopen(q, timeout=30) as r:
                    return r.status, r.read()
            except urllib.error.HTTPError as e:
                return e.code, e.read()

        def get(path):
            st, b = call(path)
            assert st == 200, (path, st, b[:300])
            return json.loads(b)

        def stop():
            nonlocal proc, log
            if proc:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
                proc = None
                log.close()

        def models():
            st, b = call('/v1/models', headers={'Authorization': 'Bearer ' + CLIENT})
            return {m['id'] for m in json.loads(b)['data']} if st == 200 else set()

        def start(ready_model):
            nonlocal proc, log
            (run / 'config.yaml').write_text(yaml.safe_dump(cfg))
            log = (run / 'cpa.log').open('w+')
            proc = subprocess.Popen([cpa, '-config', str(run / 'config.yaml')], cwd=run, env=env, stdout=log, stderr=subprocess.STDOUT)
            for _ in range(300):
                if proc.poll() is not None:
                    raise RuntimeError('CPA exited early')
                try:
                    if ready_model in models():
                        return
                except OSError:
                    pass
                time.sleep(.1)
            raise RuntimeError('readiness timeout')

        def snapshot_with(n_oauth, n_api):
            call(prefix + 'refresh', 'POST', {})
            snap = {}
            for _ in range(200):
                snap = get(prefix + 'snapshot')
                cs = [c for c in snap.get('credentials', []) if c['provider'] == 'claude']
                classified = sum(c['authType'] == 'oauth' for c in cs) >= n_oauth and sum(c['authType'] == 'api_key' for c in cs) >= n_api
                # The API model catalog can be ready before file-auth model registration.
                # Presence-only readiness captured an empty OAuth model list on amd64 CI.
                if classified and all(c.get('models') for c in cs if c['authType'] in ('oauth', 'api_key')):
                    return snap
                if classified:
                    call(prefix + 'refresh', 'POST', {})
                time.sleep(.1)
            raise RuntimeError('inventory never populated: %s' % [(c['provider'], c['authType']) for c in snap.get('credentials', [])])

        def messages(model):
            return call('/v1/messages', 'POST', {'model': model, 'max_tokens': 16, 'messages': [{'role': 'user', 'content': 'synthetic ' + str(time.monotonic_ns())}]},
                        {'Authorization': 'Bearer ' + CLIENT, 'anthropic-version': '2023-06-01'})

        def counts():
            return len(Mock.calls), len(Tripwire.hits)

        # ---- Phase A: OAuth inventory only; nothing may be completed. ----
        write_auth()
        # Readiness before any model is known: wait for any claude model in the catalog.
        (run / 'config.yaml').write_text(yaml.safe_dump(cfg))
        log = (run / 'cpa.log').open('w+')
        proc = subprocess.Popen([cpa, '-config', str(run / 'config.yaml')], cwd=run, env=env, stdout=log, stderr=subprocess.STDOUT)
        for _ in range(300):
            if proc.poll() is not None:
                raise RuntimeError('CPA exited early')
            try:
                if any(m.startswith('claude') for m in models()):
                    break
            except OSError:
                pass
            time.sleep(.1)
        else:
            raise RuntimeError('readiness timeout (phase A)')
        snap = snapshot_with(1, 0)
        oauth = next(c for c in snap['credentials'] if c['provider'] == 'claude' and c['authType'] == 'oauth')
        served = models()
        inter = sorted(m for m in oauth['models'] if m in served and m.startswith('claude') and not any(ch in m for ch in '*()'))
        check('oauth_inventory_lists_supported_claude_models', len(inter) >= 1, {'oauthModels': len(oauth['models']), 'served': len(served)})
        model = inter[0]
        check('inventory_only_no_completions', counts()[0] == 0, (counts(), Tripwire.hits))
        stop()

        # ---- Phase B: add the synthetic API key serving the same model + an API-only model. ----
        cfg['claude-api-key'] = [{'api-key': API_KEY, 'base-url': 'http://127.0.0.1:%d' % mock.server_port, 'models': [{'name': model, 'alias': model}, {'name': API_ONLY, 'alias': API_ONLY}]}]
        start(API_ONLY)
        snap = snapshot_with(1, 1)
        creds = [c for c in snap['credentials'] if c['provider'] == 'claude']
        oauth = next(c for c in creds if c['authType'] == 'oauth')
        api = next(c for c in creds if c['authType'] == 'api_key' and model in c['models'])
        check('same_model_on_oauth_and_api_credentials', model in oauth['models'] and model in api['models'] and API_ONLY in api['models'] and API_ONLY not in oauth['models'], (oauth['models'][:5], api['models']))
        check('no_real_quota_collection_configured', not qcfg.get('managementURL') and not qcfg.get('adminSources') and not qcfg.get('managementKeyEnv'))
        check('inventory_only_no_completions_with_api_key', counts()[0] == 0, counts())
        stop()

        # ---- Normalized fixture: OAuth subscription account + API account, pinned API pool. ----
        def fixture(used):
            now = datetime.datetime.now(UTC)
            s = json.loads(json.dumps(snap))
            for c in s['credentials']:
                if c['id'] == oauth['id']:
                    c['accountId'] = 'dummy-oauth-account'
                elif c['id'] == api['id']:
                    c['accountId'] = 'dummy-api-account'
            win = [{'id': wid, 'durationSeconds': dur, 'usedFraction': used, 'resetAt': (now + datetime.timedelta(hours=h)).isoformat(), 'observedAt': now.isoformat(), 'capacity': 1, 'capacitySource': 'reported'} for wid, dur, h in [('five-hour', 18000, 2), ('seven-day', 604800, 100)]]
            s['accounts'] = [{'id': 'dummy-oauth-account', 'provider': 'claude', 'label': 'Dummy OAuth (Demo User)', 'billingType': 'subscription', 'plan': 'synthetic', 'refreshStatus': 'fixture', 'observedAt': now.isoformat(), 'windows': win, 'financial': []},
                             {'id': 'dummy-api-account', 'provider': 'claude', 'label': 'Synthetic API (Demo User)', 'billingType': 'api', 'plan': 'synthetic', 'refreshStatus': 'fixture', 'observedAt': now.isoformat(), 'windows': [], 'financial': []}]
            s['pools'] = [{'id': 'api-pool', 'provider': 'claude', 'credentialIds': [api['id']]}]
            s['generatedAt'] = now.isoformat()
            (run / 'fixture.json').write_text(json.dumps(s))

        qcfg.update({'schedulerMode': 'active', 'quotaAcrossPriorities': True, 'providerStrategies': {'claude': 'quota-aware-round-robin'}, 'fixtureSnapshotPath': str(run / 'fixture.json')})

        def put_combo():
            st, b = call('/v0/management/plugins/tea-combos/combos', 'POST', {'name': PIN_COMBO, 'strategy': 'failover', 'accountStrategy': 'round-robin', 'sessionAffinity': False, 'cooldownSeconds': 0,
                                                                              'members': [{'model': model, 'provider': 'claude', 'accountPool': 'api-pool'}]})
            assert st in (200, 201), (st, b[:300])

        def three_routes(tag):
            c0 = counts()
            st, b = messages(model)
            c1 = counts()
            check(tag + '_ordinary_route_rejected', st >= 400, (st, b[:300]))
            check(tag + '_ordinary_route_no_paid_mock_request', c1 == c0, (c0, c1))
            dec = [d for d in get(prefix + 'decisions') if d.get('model') == model]
            check(tag + '_scheduler_recorded_subscription_rejection', any(d.get('mode') == 'active' and d.get('reason') == 'rejected' for d in dec), dec[-2:])
            st, b = messages(PIN_COMBO)
            c2 = counts()
            body = json.loads(b) if st == 200 else {}
            check(tag + '_pinned_api_combo_allowed', st == 200 and body.get('type') == 'message' and body['content'][0]['text'] == 'api-lane' and c2[0] == c1[0] + 1 and Mock.calls[-1]['lane'] == 'api' and Mock.calls[-1]['model'] == model and c2[1] == c1[1], (st, b[:300], c1, c2))
            st, b = messages(API_ONLY)
            c3 = counts()
            body = json.loads(b) if st == 200 else {}
            check(tag + '_api_only_model_allowed', st == 200 and body.get('type') == 'message' and body['content'][0]['text'] == 'api-lane' and c3[0] == c2[0] + 1 and Mock.calls[-1]['lane'] == 'api' and Mock.calls[-1]['model'] == API_ONLY and c3[1] == c2[1], (st, b[:300], c2, c3))

        # ---- Phase C: OAuth exhausted in snapshot, enabled in core. ----
        fixture(1.0)
        start(API_ONLY)
        put_combo()
        s = get(prefix + 'snapshot')
        oc = next(c for c in s['credentials'] if c['id'] == oauth['id'])
        check('exhausted_snapshot_loaded_oauth_enabled', not oc['disabled'] and any(w.get('usedFraction') == 1 for a in s['accounts'] if a['id'] == oc['accountId'] for w in a['windows']), oc)
        three_routes('exhausted')
        stop()

        # ---- Phase D: OAuth disabled in core; snapshot (not exhausted) still enabled. ----
        write_auth(disabled=True)
        fixture(0.1)
        start(API_ONLY)
        s = get(prefix + 'snapshot')
        oc = next(c for c in s['credentials'] if c['id'] == oauth['id'])
        check('core_disabled_snapshot_still_enabled', not oc['disabled'], oc)
        st, b = call('/v0/management/auth-files')
        files = json.loads(b).get('files', []) if st == 200 else []
        check('core_reports_oauth_disabled', any(f.get('name') == AUTH_FILE and f.get('disabled') for f in files), (st, [(f.get('name'), f.get('disabled')) for f in files]))
        three_routes('disabled')

        # ---- Global safety checks. ----
        leaked = [c for c in Mock.calls if c['lane'] != 'api' or c['oauthTokenSeen']]
        check('no_oauth_upstream_attempt_at_mock', not leaked, leaked)
        bad_hits = [h for h in Tripwire.hits if h['oauthTokenSeen'] or any(x in h['requestLine'].lower() for x in ('anthropic', 'claude'))]
        check('no_oauth_or_claude_external_attempt_at_tripwire', not bad_hits, bad_hits)
        report['upstream'] = {'mockRequests': len(Mock.calls), 'lanes': sorted({c['lane'] for c in Mock.calls}), 'paths': sorted({c['path'] for c in Mock.calls}), 'externalAttemptsRefusedByTripwire': len(Tripwire.hits)}
        report['model'] = model
        report['tripwireRequestLines'] = sorted({h['requestLine'] for h in Tripwire.hits})
        report['oauthInventoryModelCount'] = len(oauth['models'])
        report['intersectionCandidates'] = inter[:5]
    except Exception as e:
        report['error'] = '%s: %s' % (type(e).__name__, e)
        try:
            report['logTail'] = (run / 'cpa.log').read_text()[-1200:].replace(OAUTH_TOKEN, '<dummy>')
        except OSError:
            pass
    finally:
        try:
            if proc:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
            if log:
                log.close()
        finally:
            if mock:
                mock.shutdown()
                mock.server_close()
            if trip:
                trip.shutdown()
                trip.server_close()
            shutil.rmtree(run, ignore_errors=True)
        report['passed'] = sum(checks.values())
        report['total'] = len(checks)
        report['cleanedUp'] = not run.exists()
        out = ROOT / 'evidence/quota-subscription-e2e.json'
        out.parent.mkdir(exist_ok=True)
        out.write_text(json.dumps(report, indent=2))
        print(json.dumps(report, indent=2))
    ok = bool(checks) and all(checks.values()) and 'error' not in report
    return 0 if ok else 1


if __name__ == '__main__':
    raise SystemExit(main())
