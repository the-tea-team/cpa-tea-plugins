#!/bin/sh
# Cold-start proof: restart CPA (clears cooldown state), then hit ONLY the combo.
sh <spike-dir>/restart.sh > /dev/null
: > <spike-dir>/mock.log
echo "=== cold combo: members [bad-model, good-model] ==="
curl -s -m 40 -D /tmp/h.txt http://127.0.0.1:18319/v1/chat/completions \
  -H "Authorization: Bearer spike-key" -H "Content-Type: application/json" \
  -d '{"model":"spike-combo","messages":[{"role":"user","content":"hi"}]}' > /tmp/b.txt
echo "HTTP: $(head -1 /tmp/h.txt | tr -d '\r')"
echo "served-by: $(grep -o 'served-by:[a-z-]*' /tmp/b.txt)"
echo "UPSTREAM CALL ORDER (mock's own log):"
sed 's/^/   /' <spike-dir>/mock.log
echo
echo "=== recursion guard: does the combo re-enter the router? ==="
grep -ciE 'recurs|loop|re-enter' <spike-dir>/cpa.log
echo "=== second identical call (credential for bad-model now cooling) ==="
: > <spike-dir>/mock.log
curl -s -m 40 http://127.0.0.1:18319/v1/chat/completions \
  -H "Authorization: Bearer spike-key" -H "Content-Type: application/json" \
  -d '{"model":"spike-combo","messages":[{"role":"user","content":"hi"}]}' | grep -o 'served-by:[a-z-]*'
echo "UPSTREAM CALL ORDER:"
sed 's/^/   /' <spike-dir>/mock.log
