#!/bin/bash
# Measures the running bot-station container: what the browser tells
# websites (tools/fingerprint.html) and CPU / memory, idle and while
# scrolling. Usage: tools/measure.sh [label]   (needs .env's AGENT_TOKEN)
# Results go to tools/results/<label>.txt
set -e
cd "$(dirname "$0")"
label=${1:-run}
C=bot-station
TOKEN=$(grep -E '^(AGENT_TOKEN|CONTROL_TOKEN)=' ../.env | head -1 | cut -d= -f2-)
API=https://127.0.0.1:${PORT:-8443}
api() { curl -sk -m 60 -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
mkdir -p results
out=results/$label.txt
: > "$out"
say() { echo "$*" | tee -a "$out"; }

# The pages are served from inside the container (127.0.0.1:8099), so the
# test needs no network
docker exec $C mkdir -p /tmp/fp && docker exec $C chmod 755 /tmp/fp
docker exec -i $C sh -c "cat > /tmp/fp/fingerprint.html" < fingerprint.html
docker exec -i $C sh -c "cat > /tmp/fp/heavy.html" < heavy.html
docker exec -i $C sh -c "cat > /tmp/fp/webgl.html" < webgl.html
docker exec -d $C perl -MIO::Socket::INET -e '
  my $s = IO::Socket::INET->new(LocalAddr => "127.0.0.1:8099", Listen => 5, ReuseAddr => 1) or exit;
  while (my $c = $s->accept) {
    my $req = <$c>; my ($p) = $req =~ m{GET /(\S*)}; $p =~ s/[^\w.]//g;
    while (<$c>) { last if /^\r?\n$/ }
    if (open my $f, "<", "/tmp/fp/$p") { local $/; my $b = <$f>;
      print $c "HTTP/1.0 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: " . length($b) . "\r\n\r\n$b" }
    else { print $c "HTTP/1.0 404 Not Found\r\n\r\n" }
    close $c }'
sleep 0.5

cpu() { docker exec $C awk '/usage_usec/ {print $2}' /sys/fs/cgroup/cpu.stat; }
mem() { docker exec $C cat /sys/fs/cgroup/memory.current; }
# percent of one core over $1 seconds, while "$2" runs (or idles)
window() {
  local secs=$1 a b t0 t1
  a=$(cpu); t0=$(date +%s%N)
  if [ -n "$2" ]; then eval "$2"; else sleep "$secs"; fi
  b=$(cpu); t1=$(date +%s%N)
  echo $(( (b - a) * 100000 / ((t1 - t0) / 1000) ))  # in 1/1000 of a percent
}
pct() { printf '%d.%d%%' $(($1 / 1000)) $(($1 % 1000 / 100)); }

say "== $label  $(date -Is)"
say "image: $(docker image ls --format '{{.Size}}' "$(docker inspect -f '{{.Config.Image}}' $C)")"
anon() { docker exec $C awk '/^anon / {printf "%d", $2/1048576}' /sys/fs/cgroup/memory.stat; }

# Cold browser start: stop it, then time a navigation that starts it
docker exec $C pkill -u browser || true
for i in $(seq 20); do docker exec $C pgrep -u browser >/dev/null || break; sleep 0.5; done
t0=$(date +%s%N)
api -X POST $API/agent/browser/navigate -d '{"url":"http://127.0.0.1:8099/heavy.html"}' >/dev/null
say "browser cold start + first page: $(( ($(date +%s%N) - t0) / 1000000 )) ms"

api -X POST $API/agent/browser/navigate -d '{"url":"http://127.0.0.1:8099/fingerprint.html"}' >/dev/null
sleep 2
fp=$(api "$API/agent/browser/snapshot?text=20000" | sed -n 's/.*FP=\(.*\)/\1/p' | sed 's/\\"/"/g; s/"}$//; s/\\\\/\\/g')
say "fingerprint: $fp"

# Idle: browser open on a still page
api -X POST $API/agent/browser/navigate -d '{"url":"http://127.0.0.1:8099/heavy.html"}' >/dev/null
sleep 5
say "idle cpu (20 s, still page): $(pct $(window 20))"
say "memory idle: $(( $(mem) / 1048576 )) MiB total, $(anon) MiB anon (processes, excl. file cache)"

# Scrolling: 3 wheel clicks every 100 ms for 20 s, down then up
scroll='for i in $(seq 100); do d=3; [ $i -gt 50 ] && d=-3; api -X POST $API/agent/actions -d "{\"actions\":[{\"type\":\"scroll\",\"x\":640,\"y\":400,\"dy\":$d}]}" >/dev/null; sleep 0.1; done'
say "scroll cpu (heavy page): $(pct $(window 0 "$scroll"))"
say "memory after scroll: $(( $(mem) / 1048576 )) MiB total, $(anon) MiB anon, peak $(( $(docker exec $C cat /sys/fs/cgroup/memory.peak) / 1048576 )) MiB"
# Worst case: a page animating WebGL nonstop (without WebGL: an empty page)
api -X POST $API/agent/browser/navigate -d '{"url":"http://127.0.0.1:8099/webgl.html"}' >/dev/null
sleep 3
say "webgl animation cpu (15 s): $(pct $(window 15)), anon $(anon) MiB"
say "processes+threads: $(docker exec $C cat /sys/fs/cgroup/pids.current)"
docker exec $C pkill -f 'LocalAddr => "127.0.0.1:8099"' || true
