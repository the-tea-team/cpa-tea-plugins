#!/usr/bin/env python3
"""Native CPA integration for tea-combos member pins (accountId / connectionId).

Real released CPA binary (CPA_BINARY), c-shared tea-combos + tea-quota-router + tea-keys, a
synthetic quota-snapshot fixture and a local mock upstream. Mock bearer lanes high/low/other are
three native credentials; high+low share synthetic account A, other is account B. Nothing here
touches a real provider, secret or network address beyond 127.0.0.1.
"""
import concurrent.futures, datetime, http.server, importlib.util, io, json, os, pathlib, shutil
import socket, subprocess, tempfile, threading, time, urllib.error, urllib.request
import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
UTC = datetime.timezone.utc
OUT = ROOT / 'evidence/combo-pins-e2e.json'

# Reuse the existing harness Mock (stream/post-byte/cool-model behaviour) without editing it.
_spec = importlib.util.spec_from_file_location('quota_routing_e2e', ROOT / 'scripts/quota-routing-e2e.py')
_base = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_base)


class PinMock(_base.Mock):
    """Base Mock plus per-model lane failures (503 before any byte)."""
    calls = []
    lock = threading.Lock()
    fail = {}  # model -> set(lane)

    def do_POST(self):
        raw = self.rfile.read(int(self.headers['Content-Length']))
        q = json.loads(raw)
        lane = self.headers.get('Authorization', '').removeprefix('Bearer synthetic-')
        model = q['model']
        if lane in self.fail.get(model, ()):
            with self.lock:
                self.calls.append({'lane': lane, 'model': model, 'stream': q.get('stream', False)})
            body = b'{"error":{"message":"capacity unavailable"}}'
            self.send_response(503)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(body)
            return
        self.rfile = io.BytesIO(raw)
        super().do_POST()


EXPECTED = [
    'inventory_three_credentials', 'lane_identity_distinct',
    'inherit_route_unchanged_core_priority', 'account_pin_confines_to_A_both_connections',
    'account_pin_B_overrides_core_priority', 'connection_pin_exact_each_lane',
    'concurrent_same_model_A_B_inherit_isolated', 'account_failure_confined_to_A_connections',
    'account_partial_failure_retries_A_sibling_only', 'connection_failure_never_rotates_to_sibling',
    'connection_pin_healthy_sibling_unused', 'pin_plus_normal_fallback_member_allowed',
    'same_combo_same_model_pins_distinct_cooldown', 'provider_mismatch_rejects_no_upstream',
    'provider_match_explicit_ok', 'pool_intersection_narrows_pin', 'pool_disjoint_rejects_no_upstream',
    'missing_account_rejected_no_upstream', 'missing_connection_rejected_no_upstream',
    'stream_connection_first_payload_no_replay', 'stream_account_first_payload_no_replay',
    'stream_account_retry_before_first_payload', 'edited_pin_routes_to_new_target_with_session',
    'core_cooldown_final_gate_connection_pin', 'targets_requires_auth_401',
    'targets_authenticated_200_projection', 'targets_no_secret_fields',
    'exhausted_account_pin_rejected_no_upstream', 'exhausted_connection_pin_rejected_no_upstream',
    'exhausted_A_leaves_B_and_inherit_working', 'stale_snapshot_pin_rejected_no_upstream',
    'stale_snapshot_inherit_unchanged', 'targets_stale_503_bounded',
    'core_disabled_final_gate_account_and_connection', 'core_disabled_leaves_B_pin_working',
]


def main():
    checks, details = {}, {}
    report = {'syntheticOnly': True, 'checks': checks}
    proc = mock = log = None
    run = pathlib.Path(tempfile.mkdtemp(prefix='combo-pins-'))
    (run / 'plugins').mkdir()
    (run / 'auth').mkdir()
    env = dict(os.environ, GOTOOLCHAIN=os.environ.get('GOTOOLCHAIN', 'go1.27.4'), GOMAXPROCS='2', CGO_ENABLED='1')
    cpa = os.environ.get('CPA_BINARY', 'cli-proxy-api')

    def check(name, ok, detail=None):
        assert name in EXPECTED, name
        checks[name] = bool(ok)
        if not ok:
            details[name] = str(detail)[:600]

    try:
        for name in ['tea-combos', 'tea-quota-router', 'tea-keys']:
            subprocess.run(['go', 'build', '-p=2', '-buildmode=c-shared', '-o', str(run / 'plugins' / (name + '.so')), '.'],
                           cwd=ROOT / 'plugins' / name, env=env, check=True)
        mock = http.server.ThreadingHTTPServer(('127.0.0.1', 0), PinMock)
        threading.Thread(target=mock.serve_forever, daemon=True).start()
        with socket.socket() as so:
            so.bind(('127.0.0.1', 0))
            port = so.getsockname()[1]
        base = f'http://127.0.0.1:{port}'
        qprefix = '/v0/management/plugins/tea-quota-router/'
        cprefix = '/v0/management/plugins/tea-combos/'
        models = ['shared-model', 'cool-model', 'post-byte', 'fail-a', 'part-a', 'conn-fail', 'iso-fail', 'pool-model',
                  'miss-model', 'prov-model', 'sticky-model', 'stream-part', 'ex-model', 'stale-model', 'dis-model',
                  'fb-model', 'fb-fail', 'post-acct']
        lanes_prio = [('high', 10), ('low', 1), ('other', 5)]
        cfg = {'config-version': 8, 'server': {'host': '127.0.0.1', 'port': port}, 'auth-dir': str(run / 'auth'),
               'access': {'api-keys': ['synthetic-client']},
               'management': {'secret-key': 'synthetic-management', 'allow-remote': False, 'disable-control-panel': True},
               'routing': {'strategy': 'round-robin', 'session-affinity': False,
                           'retry': {'request-retry': 0, 'max-retry-credentials': 1, 'max-retry-interval': 0}},
               'plugins': {'enabled': True, 'dir': str(run / 'plugins'), 'configs': {
                   'tea-quota-router': {'enabled': True, 'dataDir': str(run / 'quota'), 'schedulerMode': 'off', 'pollIntervalSeconds': 180},
                   'tea-combos': {'enabled': True, 'statePath': str(run / 'combos.json'),
                                  'quotaSnapshotPath': str(run / 'quota/snapshot-v1.json'), 'quotaMaxAgeSeconds': 600},
                   'tea-keys': {'enabled': True, 'require_managed_keys': False, 'keys': []}}},
               'api-keys': {'openai-compatibility': [
                   {'name': 'mock', 'priority': prio, 'base-url': f'http://127.0.0.1:{mock.server_port}/v1',
                    'keys': [{'api-key': 'synthetic-' + lane}], 'models': [{'name': m, 'alias': m} for m in models]}
                   for lane, prio in lanes_prio]}}

        def call(path, method='GET', payload=None, key='synthetic-management'):
            headers = {'Content-Type': 'application/json'}
            if key:
                headers['Authorization'] = 'Bearer ' + key
            q = urllib.request.Request(base + path, method=method, headers=headers,
                                       data=json.dumps(payload).encode() if payload is not None else None)
            try:
                with urllib.request.urlopen(q, timeout=30) as r:
                    return r.status, r.read()
            except urllib.error.HTTPError as e:
                return e.code, e.read()

        def get(path):
            st, b = call(path)
            assert st == 200, (path, st, b[:500])
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
            if log:
                log.close()
                log = None

        def start():
            nonlocal proc, log
            (run / 'config.yaml').write_text(yaml.safe_dump(cfg))
            log = (run / 'cpa.log').open('w+')
            proc = subprocess.Popen([cpa, '-config', str(run / 'config.yaml')], cwd=run, env=env, stdout=log, stderr=subprocess.STDOUT)
            for _ in range(300):
                if proc.poll() is not None:
                    raise RuntimeError('CPA exited ' + (run / 'cpa.log').read_text()[-1500:])
                try:
                    st, b = call('/v1/models', key='synthetic-client')
                    if st == 200 and any(m['id'] == 'shared-model' for m in json.loads(b)['data']):
                        return
                except OSError:
                    pass
                time.sleep(.1)
            raise RuntimeError('readiness timeout')

        def restart():
            stop()
            start()

        def chat(model, stream=False, session=None):
            p = {'model': model, 'messages': [{'role': 'user', 'content': 'synthetic ' + str(time.monotonic_ns())}], 'stream': stream}
            if session:
                p['prompt_cache_key'] = session
            return call('/v1/chat/completions', 'POST', p, 'synthetic-client')

        def lane(model, session=None):
            st, b = chat(model, session=session)
            assert st == 200, (model, st, b[:800])
            return json.loads(b)['choices'][0]['message']['content']

        def upstream(model):
            return [c['lane'] for c in PinMock.calls if c['model'] == model]

        def rejected(combo_name, model, stream=False):
            """(rejected with >=400, upstream lanes seen, status) for one combo request."""
            before = len(upstream(model))
            st, b = chat(combo_name, stream)
            return st >= 400, upstream(model)[before:], (st, b[:300])

        def upsert(combo):
            st, b = call(cprefix + 'combos', 'POST', combo)
            if st == 409:
                st, b = call(cprefix + 'combos?name=' + combo['name'], 'PUT', combo)
            assert st in (200, 201), (combo['name'], st, b)

        # ---------- phase 0: discover three native credentials, fixture accounts, lane identity ----------
        start()
        call(qprefix + 'refresh', 'POST', {})
        snap = {}
        for _ in range(100):
            snap = get(qprefix + 'snapshot')
            if len(snap['credentials']) >= 3:
                break
            time.sleep(.1)
        creds = snap['credentials']
        check('inventory_three_credentials', len(creds) == 3, snap)
        assert len(creds) == 3
        provider = creds[0]['provider']
        auth_indexes = [c['authIndex'] for c in creds if c.get('authIndex')]
        acct_of = {c['id']: 'tmp-' + str(i) for i, c in enumerate(creds)}
        ids = {}
        pools = []
        windows_state = {'exhaust': False, 'stale': False}
        label = {'A': 'Account A (Demo User)', 'B': 'Account B (Demo User)'}

        def write_fixture():
            now = datetime.datetime.now(UTC)
            accounts = []
            for aid in dict.fromkeys(acct_of.values()):
                exhausted = windows_state['exhaust'] and aid == 'demo-A'
                accounts.append({'id': aid, 'provider': provider, 'label': label.get(aid[-1], 'Account ' + aid), 'billingType': 'subscription',
                                 'plan': 'synthetic', 'refreshStatus': 'fixture', 'observedAt': now.isoformat(), 'financial': [],
                                 'windows': [{'id': w, 'durationSeconds': d, 'usedFraction': 1 if exhausted else .3,
                                              'resetAt': (now + datetime.timedelta(hours=h)).isoformat(), 'observedAt': now.isoformat(),
                                              'capacity': 5, 'capacitySource': 'reported'} for w, d, h in [('five-hour', 18000, 3), ('seven-day', 604800, 100)]]})
            out = dict(snap)
            out['credentials'] = [dict(c, accountId=acct_of[c['id']],
                                       label='Connection ' + next((l for l, i in ids.items() if i == c['id']), c['id'][:6]))
                                  for c in creds]
            out['accounts'] = accounts
            out['pools'] = pools
            out['generatedAt'] = (now - datetime.timedelta(hours=2) if windows_state['stale'] else now).isoformat()
            (run / 'fixture.json').write_text(json.dumps(out))

        pools[:] = [{'id': 'one-' + str(i), 'provider': provider, 'credentialIds': [c['id']]} for i, c in enumerate(creds)]
        write_fixture()
        cfg['plugins']['configs']['tea-quota-router']['fixtureSnapshotPath'] = str(run / 'fixture.json')
        restart()

        def pool_combo(name, pool, model='shared-model'):
            return {'name': name, 'strategy': 'failover', 'accountStrategy': 'round-robin', 'cooldownSeconds': 0,
                    'members': [{'model': model, 'provider': provider, 'accountPool': pool}]}

        for i, c in enumerate(creds):
            upsert(pool_combo('identify-' + str(i), 'one-' + str(i)))
            ids[lane('identify-' + str(i))] = c['id']
        check('lane_identity_distinct', set(ids) == {'high', 'low', 'other'}, ids)
        assert set(ids) == {'high', 'low', 'other'}
        acct_of.clear()
        acct_of.update({ids['high']: 'demo-A', ids['low']: 'demo-A', ids['other']: 'demo-B'})
        pools[:] = [{'id': 'all', 'provider': provider, 'credentialIds': list(ids.values())},
                    {'id': 'one-high', 'provider': provider, 'credentialIds': [ids['high']]},
                    {'id': 'only-other', 'provider': provider, 'credentialIds': [ids['other']]}]
        write_fixture()
        restart()

        def pin(kind, value):
            return {kind: value}

        def combo(name, members, affinity=False, cooldown=0):
            return {'name': name, 'strategy': 'failover', 'sessionAffinity': affinity, 'cooldownSeconds': cooldown, 'members': members}

        def one(name, model, **pins):
            upsert(combo(name, [dict({'model': model}, **pins)]))

        A, B = 'demo-A', 'demo-B'

        # ---------- ordinary + pinned routing ----------
        upsert(combo('ordinary', [{'model': 'shared-model', 'provider': provider}]))
        inherit = [lane('ordinary') for _ in range(30)]
        check('inherit_route_unchanged_core_priority', inherit.count('high') == 30, inherit)
        one('pinA', 'shared-model', accountId=A)
        one('pinB', 'shared-model', accountId=B)
        a_lanes = [lane('pinA') for _ in range(20)]
        check('account_pin_confines_to_A_both_connections',
              set(a_lanes) == {'high', 'low'} and a_lanes.count('high') >= 8 and a_lanes.count('low') >= 8, a_lanes)
        b_lanes = [lane('pinB') for _ in range(10)]
        check('account_pin_B_overrides_core_priority', b_lanes == ['other'] * 10, b_lanes)
        exact = {}
        for l in ['high', 'low', 'other']:
            one('conn-' + l, 'shared-model', connectionId=ids[l])
            exact[l] = [lane('conn-' + l) for _ in range(6)]
        check('connection_pin_exact_each_lane', all(exact[l] == [l] * 6 for l in exact), exact)

        work = ['pinA', 'pinB', 'ordinary'] * 24
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as ex:
            res = list(ex.map(lane, work))
        ra = [r for k, r in zip(work, res) if k == 'pinA']
        rb = [r for k, r in zip(work, res) if k == 'pinB']
        ro = [r for k, r in zip(work, res) if k == 'ordinary']
        check('concurrent_same_model_A_B_inherit_isolated',
              set(ra) == {'high', 'low'} and set(rb) == {'other'} and set(ro) == {'high'}, {'a': set(ra), 'b': set(rb), 'o': set(ro)})

        # ---------- failure confinement ----------
        PinMock.fail['fail-a'] = {'high', 'low'}
        one('fail-A', 'fail-a', accountId=A)
        rej, seen, d = rejected('fail-A', 'fail-a')
        check('account_failure_confined_to_A_connections',
              rej and set(seen) == {'high', 'low'} and 'other' not in seen and len(seen) <= 8, (seen, d))
        PinMock.fail['part-a'] = {'high'}
        one('part-A', 'part-a', accountId=A)
        part = [lane('part-A') for _ in range(6)]
        check('account_partial_failure_retries_A_sibling_only', set(part) == {'low'} and 'other' not in upstream('part-a'),
              (part, upstream('part-a')))
        PinMock.fail['conn-fail'] = {'high'}
        one('cf-high', 'conn-fail', connectionId=ids['high'])
        rej, seen, d = rejected('cf-high', 'conn-fail')
        check('connection_failure_never_rotates_to_sibling', rej and seen == ['high'], (seen, d))
        one('cf-low', 'conn-fail', connectionId=ids['low'])
        cf = [lane('cf-low') for _ in range(5)]
        check('connection_pin_healthy_sibling_unused', cf == ['low'] * 5, cf)
        upsert(combo('pin-fallback', [{'model': 'fb-fail', 'connectionId': ids['high']}, {'model': 'fb-model', 'provider': provider, 'role': 'fallback'}]))
        PinMock.fail['fb-fail'] = {'high'}
        before = len(PinMock.calls)
        got = lane('pin-fallback')
        seq = [(c['model'], c['lane']) for c in PinMock.calls[before:]]
        check('pin_plus_normal_fallback_member_allowed', got == 'high' and seq == [('fb-fail', 'high'), ('fb-model', 'high')], (got, seq))
        PinMock.fail['iso-fail'] = {'high', 'low'}
        upsert(combo('dual', [{'model': 'iso-fail', 'accountId': A}, {'model': 'iso-fail', 'accountId': B}], cooldown=60))
        first = lane('dual')
        mark = len(upstream('iso-fail'))
        later = [lane('dual') for _ in range(5)]
        check('same_combo_same_model_pins_distinct_cooldown',
              first == 'other' and later == ['other'] * 5 and set(upstream('iso-fail')[mark:]) == {'other'},
              (first, later, upstream('iso-fail')))

        # ---------- provider / pool / missing ----------
        one('prov-bad', 'prov-model', accountId=A, provider='wrong-provider')
        rej, seen, d = rejected('prov-bad', 'prov-model')
        check('provider_mismatch_rejects_no_upstream', rej and not seen, (seen, d))
        one('prov-ok', 'prov-model', accountId=A, provider=provider)
        pv = lane('prov-ok')
        check('provider_match_explicit_ok', pv in ('high', 'low'), pv)
        one('pool-narrow', 'pool-model', accountId=A, accountPool='one-high')
        pn = [lane('pool-narrow') for _ in range(6)]
        check('pool_intersection_narrows_pin', pn == ['high'] * 6, pn)
        one('pool-disjoint', 'pool-model', accountId=A, accountPool='only-other')
        rej, seen, d = rejected('pool-disjoint', 'pool-model')
        check('pool_disjoint_rejects_no_upstream', rej and not seen, (seen, d))
        one('miss-acct', 'miss-model', accountId='demo-missing')
        r1 = rejected('miss-acct', 'miss-model')
        check('missing_account_rejected_no_upstream', r1[0] and not r1[1], r1)
        one('miss-conn', 'miss-model', connectionId='conn-missing')
        r2 = rejected('miss-conn', 'miss-model')
        check('missing_connection_rejected_no_upstream', r2[0] and not r2[1], r2)

        # ---------- stream ----------
        one('post-conn', 'post-byte', connectionId=ids['high'])
        before = len(upstream('post-byte'))
        st, b = chat('post-conn', True)
        time.sleep(.3)
        check('stream_connection_first_payload_no_replay', st == 200 and upstream('post-byte')[before:] == ['high'] and b'high' in b, (st, b[:300], upstream('post-byte')))
        one('post-acct-pin', 'post-acct', accountId=A)
        before = len(upstream('post-acct'))
        st, b = chat('post-acct-pin', True)
        time.sleep(.3)
        seen = upstream('post-acct')[before:]
        check('stream_account_first_payload_no_replay', st == 200 and len(seen) == 1 and seen[0] in ('high', 'low') and seen[0].encode() in b, (st, b[:300], seen))
        PinMock.fail['stream-part'] = {'high'}
        one('stream-A', 'stream-part', accountId=A)
        bodies = []
        for _ in range(6):
            st, b = chat('stream-A', True)
            bodies.append((st, b))
        check('stream_account_retry_before_first_payload',
              all(s == 200 and b'low' in b and b'high' not in b and b'other' not in b for s, b in bodies) and 'other' not in upstream('stream-part'),
              (bodies[:2], upstream('stream-part')))

        # ---------- session binding follows edited pin ----------
        sticky = lambda target: combo('sticky-pin', [dict({'model': 'sticky-model'}, **target)], affinity=True)
        upsert(sticky({'accountId': A}))
        s1 = [lane('sticky-pin', 'edit-session') for _ in range(4)]
        upsert(sticky({'accountId': B}))
        s2 = [lane('sticky-pin', 'edit-session') for _ in range(3)]
        upsert(sticky({'accountId': A}))
        s3 = [lane('sticky-pin', 'edit-session') for _ in range(3)]
        check('edited_pin_routes_to_new_target_with_session',
              len(set(s1)) == 1 and s1[0] in ('high', 'low') and s2 == ['other'] * 3 and set(s3) <= {'high', 'low'}, (s1, s2, s3))

        # ---------- core cooldown final gate (fixture still says enabled/available) ----------
        one('cool-high', 'cool-model', connectionId=ids['high'])
        first_status = chat('cool-high')[0]
        rej, seen, d = rejected('cool-high', 'cool-model')
        check('core_cooldown_final_gate_connection_pin', first_status >= 400 and rej and not seen, (first_status, seen, d))

        # ---------- targets API (parent-owned): auth + allowlist projection ----------
        tpath = cprefix + 'targets'
        st401, _ = call(tpath, key=None)
        check('targets_requires_auth_401', st401 == 401, st401)
        st200, body = call(tpath)
        proj_ok = False
        try:
            t = json.loads(body)
            conns = {c['id']: c for c in t['connections']}
            proj_ok = (st200 == 200 and set(t) == {'accounts', 'connections'}
                       and all(set(a) == {'id', 'provider', 'label'} for a in t['accounts'])
                       and all(set(c) == {'id', 'accountId', 'provider', 'label', 'models', 'disabled', 'unavailable'} for c in t['connections'])
                       and {a['id'] for a in t['accounts']} == {A, B} and set(conns) == set(ids.values())
                       and {conns[ids['high']]['accountId'], conns[ids['low']]['accountId']} == {A} and conns[ids['other']]['accountId'] == B
                       and all(isinstance(c['disabled'], bool) and isinstance(c['unavailable'], bool) and 'shared-model' in c['models'] for c in t['connections']))
        except (ValueError, KeyError, TypeError):
            pass
        check('targets_authenticated_200_projection', proj_ok, (st200, body[:300]))
        text = body.decode('utf-8', 'replace')
        banned = ['synthetic-', 'authIndex', 'nextRetryAt', 'authType', 'priority', 'windows', 'secret', 'token', 'password', '@', 'attributes', 'metadata'] + auth_indexes
        check('targets_no_secret_fields', st200 == 200 and not [x for x in banned if x in text], (st200, [x for x in banned if x in text]))

        # ---------- exhausted account (restart reloads fixture) ----------
        windows_state['exhaust'] = True
        write_fixture()
        restart()
        one('ex-A', 'ex-model', accountId=A)
        one('ex-high', 'ex-model', connectionId=ids['high'])
        ra_ = rejected('ex-A', 'ex-model')
        check('exhausted_account_pin_rejected_no_upstream', ra_[0] and not ra_[1], ra_)
        before = len(upstream('ex-model'))
        st, b = chat('ex-high')
        check('exhausted_connection_pin_rejected_no_upstream', st >= 400 and len(upstream('ex-model')) == before, (st, b[:300]))
        one('ex-B', 'ex-model', accountId=B)
        upsert(combo('ex-ordinary', [{'model': 'ex-model', 'provider': provider}]))
        check('exhausted_A_leaves_B_and_inherit_working', lane('ex-B') == 'other' and lane('ex-ordinary') == 'high',
              (upstream('ex-model')[-2:],))

        # ---------- stale snapshot ----------
        windows_state.update(exhaust=False, stale=True)
        write_fixture()
        restart()
        one('stale-A', 'stale-model', accountId=A)
        one('stale-conn', 'stale-model', connectionId=ids['other'])
        before = len(upstream('stale-model'))
        sa, sb = chat('stale-A')[0], chat('stale-conn')[0]
        check('stale_snapshot_pin_rejected_no_upstream', sa >= 400 and sb >= 400 and len(upstream('stale-model')) == before, (sa, sb))
        upsert(combo('stale-ordinary', [{'model': 'stale-model', 'provider': provider}]))
        check('stale_snapshot_inherit_unchanged', lane('stale-ordinary') == 'high', None)
        st503, tb = call(tpath)
        check('targets_stale_503_bounded', st503 == 503 and len(tb) < 300 and not any(i in tb.decode('utf-8', 'replace') for i in ids.values()), (st503, tb[:300]))

        # ---------- core disabled final gate: fixture deliberately still lists A enabled ----------
        windows_state.update(exhaust=False, stale=False)
        write_fixture()
        for entry in cfg['api-keys']['openai-compatibility']:
            if entry['keys'][0]['api-key'] in ('synthetic-high', 'synthetic-low'):
                entry['disabled'] = True
        restart()
        one('dis-A', 'dis-model', accountId=A)
        one('dis-conn', 'dis-model', connectionId=ids['high'])
        one('dis-B', 'dis-model', accountId=B)
        before = len(upstream('dis-model'))
        da, dc = chat('dis-A')[0], chat('dis-conn')[0]
        check('core_disabled_final_gate_account_and_connection', da >= 400 and dc >= 400 and len(upstream('dis-model')) == before, (da, dc, upstream('dis-model')))
        check('core_disabled_leaves_B_pin_working', lane('dis-B') == 'other', None)
    except Exception as e:
        report['error'] = repr(e)
    finally:
        if proc:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        if log:
            log.close()
        if mock:
            mock.shutdown()
            mock.server_close()
        for name in EXPECTED:
            if name not in checks:
                checks[name] = False
                details[name] = 'not run'
        report['expectedTotal'] = len(EXPECTED)
        report['total'] = len(checks)
        report['passed'] = sum(1 for n in EXPECTED if checks[n])
        if details:
            report['failures'] = details
        ok = report['passed'] == report['total'] == len(EXPECTED) and 'error' not in report
        if ok:
            shutil.rmtree(run, ignore_errors=True)
        else:
            report['runDir'] = str(run)
        OUT.write_text(json.dumps(report, indent=2))
        print(json.dumps(report, indent=2))
    return 0 if ok else 1


if __name__ == '__main__':
    raise SystemExit(main())
