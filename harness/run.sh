#!/bin/bash
# run.sh <binary> <label> <n> [extra config yaml]
# Run from a directory holding golden.db, cfg/config.yml and harness/harness.
B=$(dirname $(readlink -f $0)); bin=$1; label=$2; n=$3; extra=$4
for i in $(seq 1 $n); do
  d=$B/run-$label-$i; rm -rf $d; mkdir -p $d
  cp $B/golden.db $d/nom.db
  cp $B/cfg/config.yml $d/config.yml
  [ -n "$extra" ] && printf "%s\n" "$extra" >> $d/config.yml
  $B/harness/harness -idle 10s -screens $B/screens-$label-$i.txt -json $B/res-$label-$i.json $bin --config-path $d/config.yml > $B/res-$label-$i.txt 2>&1
done
