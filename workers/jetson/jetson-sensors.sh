#!/bin/bash
# Writes Jetson temperatures and GPU load for node_exporter's textfile collector.
# node_exporter's thermal_zone collector hangs on the powered-off cv0-2 zones,
# so each read here has a timeout and those zones are skipped.
dir=$HOME/.node-textfile
mkdir -p "$dir"
while true; do
  out=$(mktemp "$dir/.jetson.XXXX")
  {
    echo "# TYPE jetson_temp_celsius gauge"
    for z in /sys/class/thermal/thermal_zone*; do
      t=$(timeout 2 cat "$z/type" 2>/dev/null) || continue
      case $t in cv*) continue ;; esac
      v=$(timeout 2 cat "$z/temp" 2>/dev/null) || continue
      echo "jetson_temp_celsius{zone=\"${t%-thermal}\"} $(awk "BEGIN{print $v/1000}")"
    done
    l=$(timeout 2 cat /sys/devices/platform/bus@0/17000000.gpu/load 2>/dev/null) &&
      printf '# TYPE jetson_gpu_load_ratio gauge\njetson_gpu_load_ratio %s\n' "$(awk "BEGIN{print $l/1000}")"
  } >"$out"
  mv "$out" "$dir/jetson.prom"
  sleep 15
done
