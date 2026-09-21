#!/bin/sh
test "$(cat hello.txt)" = hello && echo 1 > /logs/verifier/reward.txt || echo 0 > /logs/verifier/reward.txt
