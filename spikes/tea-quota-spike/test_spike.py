import json, pathlib, subprocess, unittest

class NativeSpike(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        script = pathlib.Path(__file__).with_name('harness.py')
        result = subprocess.run(['python3', str(script)], capture_output=True, text=True, timeout=300)
        if result.returncode: raise RuntimeError(result.stderr)
        cls.report = json.loads(result.stdout)

    def test_native_loaded_and_model_inventory(self):
        self.assertTrue(self.report['native_loaded'])
        self.assertTrue(self.report['models_found'])

    def test_unpinned_priority_and_low_pin_override(self):
        r=self.report['results']
        self.assertEqual(r['normal']['upstream'],['high'])
        self.assertEqual(r['high']['upstream'],['high'])
        self.assertEqual(r['low']['upstream'],['low'])
        self.assertEqual([r[k]['status'] for k in ('normal','high','low')],[200,200,200])

    def test_caller_scope_present(self):
        for result in self.report['results'].values():
            self.assertTrue(json.loads(result['body'])['spike']['scope_present'])

    def test_gate_inventory_includes_config_credentials(self):
        self.assertGreaterEqual(self.report['inventory_count'], 2,
                                'GATE FAILED: host.auth.list omitted configured API credentials')

if __name__ == '__main__': unittest.main()
