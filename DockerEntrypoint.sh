#!/bin/sh

# Start fail2ban with the 3x-ipl jail
if [ "$XUI_ENABLE_FAIL2BAN" = "true" ]; then
    LOG_FOLDER="${XUI_LOG_FOLDER:-/var/log/x-ui}"
    mkdir -p "$LOG_FOLDER"
    touch "$LOG_FOLDER/3xipl.log" "$LOG_FOLDER/3xipl-banned.log"

    mkdir -p /etc/fail2ban/jail.d /etc/fail2ban/filter.d /etc/fail2ban/action.d

    cat > /etc/fail2ban/jail.d/3x-ipl.conf << EOF
[3x-ipl]
enabled=true
backend=auto
filter=3x-ipl
action=3x-ipl
logpath=$LOG_FOLDER/3xipl.log
maxretry=1
findtime=32
bantime=30m
EOF

    cat > /etc/fail2ban/filter.d/3x-ipl.conf << 'EOF'
[Definition]
datepattern = ^%%Y/%%m/%%d %%H:%%M:%%S
failregex   = \[LIMIT_IP\]\s*Email\s*=\s*<F-USER>.+</F-USER>\s*\|\|\s*Disconnecting OLD IP\s*=\s*<ADDR>\s*\|\|\s*Timestamp\s*=\s*\d+
ignoreregex =
EOF

    # Ports to exempt from the ban so an over-limit proxy client can never lock
    # the administrator out of SSH or the panel. The ban still covers every other
    # TCP port (including all Xray inbounds), so IP-limit keeps working for inbounds
    # added later without regenerating these files.
    SSH_PORTS=$(grep -oE '^[[:space:]]*Port[[:space:]]+[0-9]+' /etc/ssh/sshd_config 2>/dev/null | grep -oE '[0-9]+' | paste -sd, -)
    [ -z "$SSH_PORTS" ] && SSH_PORTS="22"
    PANEL_PORT=$(/app/x-ui setting -show true 2>/dev/null | grep -Eo 'port: .+' | awk '{print $2}')
    EXEMPT_PORTS="$SSH_PORTS"
    [ -n "$PANEL_PORT" ] && EXEMPT_PORTS="$EXEMPT_PORTS,$PANEL_PORT"

    cat > /etc/fail2ban/action.d/3x-ipl.conf << EOF
[INCLUDES]
before = iptables-allports.conf

[Definition]
actionstart = <iptables> -N f2b-<name>
              <iptables> -A f2b-<name> -j <returntype>
              <iptables> -I <chain> -p <protocol> -j f2b-<name>

actionstop = <iptables> -D <chain> -p <protocol> -j f2b-<name>
             <actionflush>
             <iptables> -X f2b-<name>

actioncheck = <iptables> -n -L <chain> | grep -q 'f2b-<name>[ \t]'

actionban = <iptables> -I f2b-<name> 1 -s <ip> -p <protocol> -m multiport ! --dports <exemptports> -j <blocktype>
            echo "\$(date +"%%Y/%%m/%%d %%H:%%M:%%S")   BAN   [Email] = <F-USER> [IP] = <ip> banned for <bantime> seconds." >> $LOG_FOLDER/3xipl-banned.log

actionunban = <iptables> -D f2b-<name> -s <ip> -p <protocol> -m multiport ! --dports <exemptports> -j <blocktype>
              echo "\$(date +"%%Y/%%m/%%d %%H:%%M:%%S")   UNBAN   [Email] = <F-USER> [IP] = <ip> unbanned." >> $LOG_FOLDER/3xipl-banned.log

[Init]
name = default
protocol = tcp
chain = INPUT
exemptports = $EXEMPT_PORTS
EOF

    fail2ban-client -x start
fi

# Start the separately configured, loopback-only Simurgh panel. Fresh Docker
# data volumes require PHOENIX_SIMURGH_ROLE=exit (foreign) or relay (Iran);
# existing Simurgh configs remain authoritative. Foreign-role listeners stay
# disabled until an administrator configures them.
if [ -x /opt/phoenix-simurgh/simurgh ] && [ -f /opt/phoenix-simurgh/simurgh-role.py ] \
    && command -v python3 >/dev/null 2>&1; then
    mkdir -p /etc/phoenix-simurgh /var/log/phoenix-simurgh
    chmod 700 /etc/phoenix-simurgh
    role_info="$(SIMURGH_HOME=/etc/phoenix-simurgh PYTHONPATH=/opt/phoenix-simurgh/lib${PYTHONPATH:+:$PYTHONPATH} \
        python3 /opt/phoenix-simurgh/simurgh-role.py \
        --home /etc/phoenix-simurgh --json 2>/dev/null || true)"
    SIMURGH_ROLE="$(printf '%s' "$role_info" | python3 -c 'import json,sys; print(json.load(sys.stdin)["role"])' 2>/dev/null || true)"
    MIGRATE_RELAY="$(printf '%s' "$role_info" | python3 -c 'import json,sys; print(str(json.load(sys.stdin)["migrate_relay"]).lower())' 2>/dev/null || true)"
    case "$SIMURGH_ROLE" in
        exit|relay)
            echo "[Simurgh] Selected or retained role: $SIMURGH_ROLE."
            ;;
        *)
            echo "[Simurgh] No role selected. For a new data volume set PHOENIX_SIMURGH_ROLE=exit (foreign) or relay (Iran); PHOENIX will still start without Simurgh."
            SIMURGH_ROLE=""
            ;;
    esac
    if [ -n "$SIMURGH_ROLE" ]; then
        PHOENIX_SIMURGH_ROLE="$SIMURGH_ROLE"
        export PHOENIX_SIMURGH_ROLE
    fi

    SIMURGH_CONFIG_OK=true
    SIMURGH_CONFIG_CREATED=false
    SIMURGH_CONFIG_EXISTED=false
    if [ -n "$SIMURGH_ROLE" ] && [ -s "/etc/phoenix-simurgh/$SIMURGH_ROLE.toml" ]; then
        SIMURGH_CONFIG_EXISTED=true
    fi
    if [ -n "$SIMURGH_ROLE" ] && [ "$SIMURGH_CONFIG_EXISTED" = false ]; then
        if [ "$SIMURGH_ROLE" = "relay" ]; then
            if ! SIMURGH_HOME=/etc/phoenix-simurgh /opt/phoenix-simurgh/simurgh install \
                --role relay --pair-later --panel-port 18787 --engine python \
                --no-units --no-enable --no-start --force \
                >> /var/log/phoenix-simurgh/setup.log 2>&1; then
                echo "[Simurgh] Relay configuration failed; PHOENIX will still start."
                SIMURGH_CONFIG_OK=false
            fi
        else
            if ! SIMURGH_HOME=/etc/phoenix-simurgh /opt/phoenix-simurgh/simurgh install \
                --role exit --panel-port 18787 --engine python \
                --no-units --no-enable --no-start --force \
                >> /var/log/phoenix-simurgh/setup.log 2>&1; then
                echo "[Simurgh] Foreign-role configuration failed; PHOENIX will still start."
                SIMURGH_CONFIG_OK=false
            elif ! SIMURGH_HOME=/etc/phoenix-simurgh PYTHONPATH=/opt/phoenix-simurgh/lib \
                python3 - /etc/phoenix-simurgh/exit.toml >> /var/log/phoenix-simurgh/setup.log 2>&1 <<'PY'
import sys
from simurgh.config import load_exit, save_exit
config = load_exit(sys.argv[1])
for listener in config.listen:
    listener.enabled = False
config.push_ports = []
config.push_enabled = False
save_exit(config, sys.argv[1])
PY
            then
                echo "[Simurgh] Could not safely leave the foreign-role tunnel listeners disabled; PHOENIX will still start."
                SIMURGH_CONFIG_OK=false
            fi
        fi
    fi
    if [ "$SIMURGH_CONFIG_OK" = true ] && [ "$SIMURGH_CONFIG_EXISTED" = false ] \
        && [ -s "/etc/phoenix-simurgh/$SIMURGH_ROLE.toml" ]; then
        SIMURGH_CONFIG_CREATED=true
    fi

    if [ "$SIMURGH_CONFIG_OK" = true ] && [ -n "$SIMURGH_ROLE" ]; then
        if [ "$MIGRATE_RELAY" = "true" ] && [ -s /etc/phoenix-simurgh/relay.toml ]; then
            mkdir -p /etc/phoenix-simurgh/phoenix-backup
            chmod 700 /etc/phoenix-simurgh/phoenix-backup
            if mv /etc/phoenix-simurgh/relay.toml \
                "/etc/phoenix-simurgh/phoenix-backup/relay.toml.before-role-migration.$(date +%s)"; then
                echo "[Simurgh] Preserved the old idle relay config and selected the detected foreign role."
            else
                echo "[Simurgh] Could not preserve the old relay config; keeping PHOENIX available without Simurgh."
                if [ "$SIMURGH_CONFIG_CREATED" = true ]; then
                    rm -f /etc/phoenix-simurgh/exit.toml
                fi
                SIMURGH_CONFIG_OK=false
            fi
        fi
    fi

    if [ "$SIMURGH_CONFIG_OK" = true ] && [ -n "$SIMURGH_ROLE" ]; then
        printf '%s\n' "$SIMURGH_ROLE" > /etc/phoenix-simurgh/phoenix-role.tmp
        chmod 600 /etc/phoenix-simurgh/phoenix-role.tmp
        mv -f /etc/phoenix-simurgh/phoenix-role.tmp /etc/phoenix-simurgh/phoenix-role
        SIMURGH_LOG="/var/log/phoenix-simurgh/$SIMURGH_ROLE.log"
        SIMURGH_HOME=/etc/phoenix-simurgh PHOENIX_SIMURGH_ROLE="$SIMURGH_ROLE" \
            /opt/phoenix-simurgh/simurgh "$SIMURGH_ROLE" \
            --panel-host 127.0.0.1 --panel-port 18787 >> "$SIMURGH_LOG" 2>&1 &

        # Verify the selected role and its UI assets in the background, without
        # delaying PHOENIX's own panel startup.
        (
            export PHOENIX_SIMURGH_ROLE="$SIMURGH_ROLE"
            attempt=0
            while [ "$attempt" -lt 30 ]; do
                health="$(curl --noproxy '*' -fsS --max-time 1 http://127.0.0.1:18787/healthz 2>/dev/null || true)"
                if [ -n "$health" ] && printf '%s' "$health" | python3 -c 'import json,os,sys; d=json.load(sys.stdin); raise SystemExit(0 if d.get("ok") is True and d.get("product") == "Simurgh Tunnel" and d.get("role") == os.environ["PHOENIX_SIMURGH_ROLE"] else 1)' 2>/dev/null \
                    && curl --noproxy '*' -fsS --max-time 2 http://127.0.0.1:18787/app.js -o /dev/null 2>/dev/null \
                    && curl --noproxy '*' -fsS --max-time 2 http://127.0.0.1:18787/app.css -o /dev/null 2>/dev/null; then
                    echo "[Simurgh] $SIMURGH_ROLE service and panel assets are healthy."
                    exit 0
                fi
                attempt=$((attempt + 1))
                sleep 1
            done
            echo "[Simurgh] $SIMURGH_ROLE health/UI check failed; see setup.log and $SIMURGH_LOG. PHOENIX remains available."
        ) &
    fi
fi

# Run PHOENIX as the container's primary process.
exec /app/x-ui
