#!/bin/sh
# Drive the three combo scenarios and show proof of call order from the mock upstream's own log.
call() {
  name="$1"
  : > <spike-dir>/mock.log
  echo "=============== $name ==============="
  curl -s -m 40 -D /tmp/h.txt http://127.0.0.1:18319/v1/chat/completions \
    -H "Authorization: Bearer spike-key" -H "Content-Type: application/json" \
    -d "{\"model\":\"$name\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" > /tmp/b.txt
  echo "HTTP: $(head -1 /tmp/h.txt | tr -d '\r')"
  echo "BODY: $(head -c 400 /tmp/b.txt)"
  echo "UPSTREAM CALL ORDER (mock log):"
  sed 's/^/   /' <spike-dir>/mock.log
  grep -i 'x-tea-combo' /tmp/h.txt | sed 's/^/HDR: /'
}
echo "### sanity: direct good-model, direct bad-model"
call good-model
call bad-model
echo
echo "### the combos"
call spike-combo
call spike-allbad
call spike-stop
