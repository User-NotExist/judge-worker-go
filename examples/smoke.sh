#!/usr/bin/env bash
# End-to-end smoke test against the dev stack (deploy/dev/compose.yaml):
#   docker compose -f deploy/dev/compose.yaml up -d --build
#   examples/smoke.sh
# Each case enqueues a job via devtool and checks the verdict / score in the done event.
set -uo pipefail
cd "$(dirname "$0")/.."
DC=(docker compose -f deploy/dev/compose.yaml --profile tools)
P=/examples/problems S=/examples/solutions
pass=0 fail=0 id=$(( $(date +%s) % 100000 * 100 ))

check() { # name expect_verdict expect_score_milli devtool-args...
  local name=$1 want=$2 score=$3; shift 3
  id=$((id + 1))
  "${DC[@]}" run --rm -T devtool submit -id "$id" "$@" >/dev/null || { echo "FAIL $name: enqueue"; fail=$((fail+1)); return; }
  local done
  done=$("${DC[@]}" run --rm -T devtool wait -id "$id" -timeout 180s 2>&1)
  local v s
  v=$(echo "$done" | python3 -c 'import json,sys; print(json.load(sys.stdin)["verdict"])' 2>/dev/null)
  s=$(echo "$done" | python3 -c 'import json,sys; print(json.load(sys.stdin)["score_milli"])' 2>/dev/null)
  if [[ "$v" == "$want" && ( "$score" == "-" || "$s" == "$score" ) ]]; then
    printf 'ok   %-36s %-4s %s\n' "$name" "$v" "$s"; pass=$((pass+1))
  else
    printf 'FAIL %-36s got %s %s, want %s %s  %s\n' "$name" "$v" "$s" "$want" "$score" "$done"; fail=$((fail+1))
  fi
}

check "a+b C++ AC"                AC  100000 -problem $P/aplusb -lang cpp20-gcc13 -files $S/aplusb/ac.cpp -lane exam
check "a+b Python AC"             AC  100000 -problem $P/aplusb -lang py312 -files $S/aplusb/ac.py
check "a+b int overflow (30 pts)" WA  30000  -problem $P/aplusb -lang cpp20-gcc13 -files $S/aplusb/int32.cpp
check "a+b WA"                    WA  0      -problem $P/aplusb -lang cpp20-gcc13 -files $S/aplusb/wa.cpp
check "a+b CE"                    CE  0      -problem $P/aplusb -lang cpp20-gcc13 -files $S/aplusb/ce.cpp
check "a+b RE"                    RE  0      -problem $P/aplusb -lang cpp20-gcc13 -files $S/aplusb/re.cpp
check "a+b TLE"                   TLE 0      -problem $P/aplusb -lang cpp20-gcc13 -files $S/aplusb/tle.cpp
check "a+b MLE"                   MLE 0      -problem $P/aplusb -lang cpp20-gcc13 -files $S/aplusb/mle.cpp
check "a+b OLE"                   OLE 0      -problem $P/aplusb -lang py312 -files $S/aplusb/ole.py
check "strict: exact layout AC"     AC  100000 -problem $P/aplusb-strict -lang cpp20-gcc13 -files $S/aplusb/ac.cpp
check "strict: no final newline AC"  AC  100000 -problem $P/aplusb-strict -lang py312 -files $S/aplusb/no_newline.py
check "strict: trailing space PE"    PE  0      -problem $P/aplusb-strict -lang cpp20-gcc13 -files $S/aplusb/trailing_space.cpp
check "strict: wrong answer stays WA" WA 0      -problem $P/aplusb-strict -lang cpp20-gcc13 -files $S/aplusb/wa.cpp
check "non-strict: trailing space AC" AC 100000 -problem $P/aplusb -lang cpp20-gcc13 -files $S/aplusb/trailing_space.cpp
check "testlib partial AC"        AC  100000 -problem $P/partial -lang py312 -files $S/partial/ac.py
check "testlib partial points"    WA  10000  -problem $P/partial -lang py312 -files $S/partial/half.py
check "interactive AC"            AC  100000 -problem $P/interactive -lang cpp20-gcc13 -files $S/interactive/ac.cpp
check "interactive WA"            WA  0      -problem $P/interactive -lang cpp20-gcc13 -files $S/interactive/wa.cpp
check "interactive TLE"           TLE 0      -problem $P/interactive -lang py312 -files $S/interactive/tle.py
check "output-only (1 of 2)"      WA  40000  -problem $P/outputonly -lang output-only -files $S/outputonly/1.out
check "function task C++ clean"   AC  100000 -problem $P/function -lang cpp20-gcc13 -files $S/function/solution_clean.cpp,$S/function/harness.cpp -names main.cpp,harness.cpp
check "function task stray print" WA  0      -problem $P/function -lang cpp20-gcc13 -files $S/function/solution.cpp,$S/function/harness.cpp -names main.cpp,harness.cpp
check "function task Python"      AC  100000 -problem $P/function -lang py312 -files $S/function/harness.py,$S/function/solution.py -names main.py,solution.py

echo "passed $pass, failed $fail"
[[ $fail == 0 ]]
