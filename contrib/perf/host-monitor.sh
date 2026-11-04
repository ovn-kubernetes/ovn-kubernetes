#!/usr/bin/env bash

# Temp monitoring script while we scale the workloads

set -u

OUTPUT_DIR=""
INTERVAL=10
DURATION=0
TOP_N=50
PROCS_EVERY=30
EVENT_NAME=""

usage() {
    cat <<'EOF'
    temp monitor script 
EOF
}

die() {
    echo "host-monitor.sh: $*" >&2
    exit 1
}

while [ $# -gt 0 ]; do
    case "$1" in
        --output) [ $# -ge 2 ] || die "--output requires a value"; OUTPUT_DIR="$2"; shift 2 ;;
        --interval) [ $# -ge 2 ] || die "--interval requires a value"; INTERVAL="$2"; shift 2 ;;
        --duration) [ $# -ge 2 ] || die "--duration requires a value"; DURATION="$2"; shift 2 ;;
        --top-n) [ $# -ge 2 ] || die "--top-n requires a value"; TOP_N="$2"; shift 2 ;;
        --procs-every) [ $# -ge 2 ] || die "--procs-every requires a value"; PROCS_EVERY="$2"; shift 2 ;;
        event)
            [ $# -ge 2 ] || die "event requires a name"
            EVENT_NAME="$2"
            shift 2
            ;;
        -h|--help) usage; exit 0 ;;
        *) usage; die "unknown argument: $1" ;;
    esac
done

[ -n "$OUTPUT_DIR" ] || { usage; die "--output is required"; }
case "$INTERVAL" in (*[!0-9]*|'') die "--interval must be a positive integer";; esac
[ "$INTERVAL" -ge 1 ] || die "--interval must be >= 1"
case "$TOP_N" in (*[!0-9]*|'') die "--top-n must be a positive integer";; esac
case "$PROCS_EVERY" in (*[!0-9]*|'') die "--procs-every must be a positive integer";; esac

# --- event subcommand: append a phase marker --------------------------------
if [ -n "$EVENT_NAME" ]; then
    [ -d "$OUTPUT_DIR" ] || die "output dir $OUTPUT_DIR does not exist"
    [ -f "$OUTPUT_DIR/events.tsv" ] || printf 'epoch\tevent\n' > "$OUTPUT_DIR/events.tsv"
    printf '%s\t%s\n' "$(date +%s)" "$EVENT_NAME" >> "$OUTPUT_DIR/events.tsv"
    exit 0
fi

# --- sampler ----------------------------------------------------------------
for tool in pidstat iostat; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool not found; install sysstat (apt-get install sysstat)"
done

[ -e "$OUTPUT_DIR" ] && { [ -d "$OUTPUT_DIR" ] && [ -z "$(ls -A "$OUTPUT_DIR")" ] || die "output dir $OUTPUT_DIR exists and is not empty; remove it or pick another path"; }
mkdir -p "$OUTPUT_DIR"

CPU_FILE="$OUTPUT_DIR/cpu.tsv"
MEM_FILE="$OUTPUT_DIR/memory.tsv"
PSI_FILE="$OUTPUT_DIR/psi.tsv"
PROC_CPU_FILE="$OUTPUT_DIR/proc-cpu.tsv"
PROC_MEM_FILE="$OUTPUT_DIR/proc-mem.tsv"
IO_FILE="$OUTPUT_DIR/io.tsv"
NET_FILE="$OUTPUT_DIR/net.tsv"
EVENTS_FILE="$OUTPUT_DIR/events.tsv"
META_FILE="$OUTPUT_DIR/meta.json"

printf 'epoch\tcpu\tusr\tnice\tsys\tiowait\tirq\tsoftirq\tsteal\tidle\n' > "$CPU_FILE"
printf 'epoch\tmem_total_kb\tmem_free_kb\tmem_available_kb\tbuffers_kb\tcached_kb\tslab_kb\tswap_total_kb\tswap_used_kb\tpswpin_pages\tpswpout_pages\n' > "$MEM_FILE"
printf 'epoch\tresource\tsome_avg10\tsome_avg60\tfull_avg10\n' > "$PSI_FILE"
printf 'epoch\tpid\tcomm\tcpu_pct\n' > "$PROC_CPU_FILE"
printf 'epoch\tpid\tcomm\trss_kb\tmem_pct\n' > "$PROC_MEM_FILE"
printf 'epoch\tdev\tr_s\tw_s\trmb_s\twmb_s\tr_await_ms\tw_await_ms\tutil_pct\n' > "$IO_FILE"
printf 'epoch\tiface\trx_bps\trx_pps\trx_drop_pps\ttx_bps\ttx_pps\ttx_drop_pps\n' > "$NET_FILE"
printf 'epoch\tevent\n' > "$EVENTS_FILE"

RUN_START=$(date +%s)
write_meta() {
    cat > "$META_FILE" <<EOF
{
  "host": "$(hostname)",
  "kernel": "$(uname -r)",
  "arch": "$(uname -m)",
  "cpu_model": "$(awk -F': ' '/model name/ {print $2; exit}' /proc/cpuinfo)",
  "ncpu": $(nproc),
  "mem_total_kb": $(awk '/^MemTotal:/ {print $2}' /proc/meminfo),
  "interval": $INTERVAL,
  "top_n": $TOP_N,
  "procs_every": $PROCS_EVERY,
  "start": $RUN_START,
  "end": ${1:-null}
}
EOF
}
write_meta

STOP=0
WARNINGS=0
warn() {
    WARNINGS=$((WARNINGS + 1))
    echo "host-monitor.sh: warning: $*" >&2
    if [ "$((WARNINGS % 30))" -eq 0 ]; then
        echo "host-monitor.sh: $WARNINGS warnings so far, continuing" >&2
    fi
}

on_exit() {
    write_meta "$(date +%s)"
    printf '%s\t%s\n' "$(date +%s)" "monitor-stopped" >> "$EVENTS_FILE"
    echo "host-monitor.sh: monitoring stopped, data in $OUTPUT_DIR"
}
trap on_exit EXIT
trap 'STOP=1' TERM INT

# sample_net_raw: two /proc/net/dev snapshots, one second apart, bracketed by
# epoch-nanosecond markers so the parser can compute exact per-second rates.
sample_net_raw() {
    local t
    t=$(date +%s%N)
    echo "@$t"
    cat /proc/net/dev
    sleep 1
    t=$(date +%s%N)
    echo "@$t"
    cat /proc/net/dev
}

# sample_cpu_raw: two /proc/stat cpu-counter snapshots, one second apart.
# mpstat is avoided on purpose: its output format (and -P ALL behavior on
# NUMA systems) varies too much across sysstat versions.
sample_cpu_raw() {
    local t
    t=$(date +%s%N)
    echo "@$t"
    awk '$1 ~ /^cpu/ { print }' /proc/stat
    sleep 1
    t=$(date +%s%N)
    echo "@$t"
    awk '$1 ~ /^cpu/ { print }' /proc/stat
}

# sample_round: run all the one-second sysstat samplers in parallel (count=2
# because their first report is since-boot averages; only the second block is
# kept), then parse each snapshot into the flat files.
sample_round() {
    local ts=$1 tmp
    tmp=$(mktemp -d)
    ( sample_cpu_raw > "$tmp/cpu" 2>&1 ) &
    ( pidstat -h 1 2 > "$tmp/pidstat" 2>&1 ) &
    ( pidstat -h -r 1 2 > "$tmp/pidstat-r" 2>&1 ) &
    ( iostat -xdz 1 2 > "$tmp/iostat" 2>&1 ) &
    ( sample_net_raw > "$tmp/net" 2>&1 ) &
    wait

    # cpu.tsv: per-CPU percentages from the /proc/stat delta; the "cpu"
    # aggregate row is emitted as "all". Guest time folds into user.
    [ -s "$tmp/cpu" ] || warn "failed to read /proc/stat"
    awk -v ts="$ts" '
        /^@/ { n++; next }
        n == 1 {
            present[$1] = 1
            for (i = 2; i <= 11; i++) v[$1, i] = $i
            next
        }
        n == 2 && present[$1] {
            tot = 0
            for (i = 2; i <= 11; i++) { d = $i - v[$1, i]; dd[i] = d; tot += d }
            if (tot <= 0) next
            name = ($1 == "cpu") ? "all" : substr($1, 4)
            # /proc/stat order: user nice system idle iowait irq softirq steal
            # guest(10) folds into usr, guest_nice(11) into nice.
            printf "%s\t%s\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\n", ts, name, \
                (dd[2] + dd[10]) * 100 / tot, (dd[3] + dd[11]) * 100 / tot, dd[4] * 100 / tot, \
                dd[6] * 100 / tot, dd[7] * 100 / tot, dd[8] * 100 / tot, dd[9] * 100 / tot, \
                dd[5] * 100 / tot
        }
    ' "$tmp/cpu" >> "$CPU_FILE" || warn "failed to parse /proc/stat"

    # proc-cpu.tsv: pidstat's layout varies across sysstat versions, so the
    # columns are located by name from the block header. 12.x prefixes the
    # header with "#", which shifts its names one column right of the data;
    # older versions print a bare time-prefixed header (names align 1:1).
    # Only the second block (per-interval) is kept.
    [ -s "$tmp/pidstat" ] || warn "pidstat produced no output"
    awk -v ts="$ts" '
        /^#/ { iter++; for (i = 2; i <= NF; i++) col[$i] = i - 1; next }
        $1 ~ /^[0-9]+:[0-9]+:[0-9]+$/ && $2 == "UID" { iter++; for (i = 2; i <= NF; i++) col[$i] = i; next }
        iter == 2 && NF >= 4 {
            pc = col["PID"]; cc = col["%CPU"]; mc = col["Command"]
            if (!mc) mc = col["COMMAND"]
            if (pc && cc && mc) printf "%s\t%s\t%s\t%s\n", ts, $pc, $mc, $cc
        }
    ' "$tmp/pidstat" | sort -t $'\t' -k4,4nr > "$tmp/proc-cpu" \
        || warn "failed to parse pidstat cpu output"

    # proc-mem.tsv: same approach as proc-cpu.tsv
    [ -s "$tmp/pidstat-r" ] || warn "pidstat -r produced no output"
    awk -v ts="$ts" '
        /^#/ { iter++; for (i = 2; i <= NF; i++) col[$i] = i - 1; next }
        $1 ~ /^[0-9]+:[0-9]+:[0-9]+$/ && $2 == "UID" { iter++; for (i = 2; i <= NF; i++) col[$i] = i; next }
        iter == 2 && NF >= 5 {
            pc = col["PID"]; rc = col["RSS"]; mc = col["%MEM"]; cc = col["Command"]
            if (!cc) cc = col["COMMAND"]
            if (pc && rc && mc && cc) printf "%s\t%s\t%s\t%s\t%s\n", ts, $pc, $cc, $rc, $mc
        }
    ' "$tmp/pidstat-r" | sort -t $'\t' -k4,4nr > "$tmp/proc-mem" \
        || warn "failed to parse pidstat -r output"

    # io.tsv: locate columns by header name (layout varies across sysstat
    # versions: "Device:" vs "Device", rMB/s vs rkB/s, await vs r_/w_await),
    # keep only the second (per-interval) report block.
    [ -s "$tmp/iostat" ] || warn "iostat produced no output"
    awk -v ts="$ts" '
        $1 == "Device" || $1 == "Device:" {
            hdr++
            delete col
            for (i = 2; i <= NF; i++) col[$i] = i
            next
        }
        hdr == 2 && NF >= 5 && $2 ~ /^[0-9.]+$/ {
            if (col["rMB/s"]) { rmb = $col["rMB/s"]; wmb = $col["wMB/s"] }
            else if (col["rkB/s"]) { rmb = $col["rkB/s"] / 1024; wmb = $col["wkB/s"] / 1024 }
            else { rmb = ""; wmb = "" }
            if (col["await"]) { ra = $col["await"]; wa = $col["await"] }
            else { ra = $col["r_await"]; wa = $col["w_await"] }
            printf "%s\t%s\t%s\t%s\t%.3f\t%.3f\t%s\t%s\t%s\n", ts, $1, $col["r/s"], $col["w/s"], rmb, wmb, ra, wa, $col["%util"]
        }
    ' "$tmp/iostat" >> "$IO_FILE" || warn "failed to parse iostat output"

    # net.tsv: per-interface byte/packet/drop rates from the two snapshots
    [ -s "$tmp/net" ] || warn "failed to read /proc/net/dev"
    awk -v ts="$ts" '
        /^@/ { n++; tt[n] = substr($0, 2); next }
        $1 ~ /^Inter-|^face/ { next }
        NF < 11 { next }
        {
            i = $1; sub(/:$/, "", i)
            if (n == 1) { rxb[i] = $2; rxp[i] = $3; rxd[i] = $5; txb[i] = $10; txp[i] = $11; txd[i] = $13 }
            else if (n == 2 && (i in rxb)) {
                dt = (tt[2] - tt[1]) / 1000000000
                if (dt < 1) dt = 1
                printf "%s\t%s\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\n", ts, i, \
                    ($2 - rxb[i]) / dt, ($3 - rxp[i]) / dt, ($5 - rxd[i]) / dt, \
                    ($10 - txb[i]) / dt, ($11 - txp[i]) / dt, ($13 - txd[i]) / dt
            }
        }
    ' "$tmp/net" >> "$NET_FILE" || warn "failed to parse /proc/net/dev"

    # memory.tsv: /proc/meminfo + swap-in/out counters from /proc/vmstat
    awk -v ts="$ts" '
        FILENAME ~ /meminfo/ && /^MemTotal:/     { t = $2 }
        FILENAME ~ /meminfo/ && /^MemFree:/      { f = $2 }
        FILENAME ~ /meminfo/ && /^MemAvailable:/ { a = $2 }
        FILENAME ~ /meminfo/ && /^Buffers:/      { b = $2 }
        FILENAME ~ /meminfo/ && /^Cached:/       { c = $2 }
        FILENAME ~ /meminfo/ && /^Slab:/         { s = $2 }
        FILENAME ~ /meminfo/ && /^SwapTotal:/    { st = $2 }
        FILENAME ~ /meminfo/ && /^SwapFree:/     { sf = $2 }
        FILENAME ~ /vmstat/ && /^pswpin/         { pin = $2 }
        FILENAME ~ /vmstat/ && /^pswpout/        { pout = $2 }
        END { printf "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", ts, t, f, a, b, c, s, st, st - sf, pin, pout }
    ' /proc/meminfo /proc/vmstat >> "$MEM_FILE" || warn "failed to read memory info"

    # psi.tsv: pressure-stall information (already per-window averages)
    for res in cpu memory io; do
        awk -v ts="$ts" -v res="$res" '
            /^some/ { s10 = $2; sub(/^avg10=/, "", s10); s60 = $3; sub(/^avg60=/, "", s60) }
            /^full/ { f10 = $2; sub(/^avg10=/, "", f10) }
            END { printf "%s\t%s\t%s\t%s\t%s\n", ts, res, s10, s60, f10 }
        ' "/proc/pressure/$res" >> "$PSI_FILE" 2>/dev/null || warn "failed to read /proc/pressure/$res"
    done

    # process snapshots: top-N per round, full list every PROCS_EVERY rounds
    if [ "$((ROUND % PROCS_EVERY))" -eq 0 ]; then
        cat "$tmp/proc-cpu" >> "$PROC_CPU_FILE"
        cat "$tmp/proc-mem" >> "$PROC_MEM_FILE"
    else
        head -n "$TOP_N" "$tmp/proc-cpu" >> "$PROC_CPU_FILE"
        head -n "$TOP_N" "$tmp/proc-mem" >> "$PROC_MEM_FILE"
    fi

    rm -rf "$tmp"
}

echo "host-monitor.sh: monitoring $OUTPUT_DIR every ${INTERVAL}s (top-n=$TOP_N, full proc list every $PROCS_EVERY samples)"
ROUND=0
while [ "$STOP" -eq 0 ]; do
    t_start=$(date +%s)
    ts=$t_start
    sample_round "$ts"
    ROUND=$((ROUND + 1))

    if [ "$DURATION" -gt 0 ] && [ $((t_start - RUN_START)) -ge "$DURATION" ]; then
        break
    fi

    t_end=$(date +%s)
    sleep_for=$((INTERVAL - (t_end - t_start)))
    if [ "$STOP" -eq 0 ] && [ "$sleep_for" -gt 0 ]; then
        sleep "$sleep_for"
    fi
done

exit 0
