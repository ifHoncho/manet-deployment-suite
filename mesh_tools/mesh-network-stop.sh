#!/bin/bash

# Source config file if it exists
if [ -f /etc/mesh-network/mesh-config.conf ]; then
    # Filter out comments and lines starting with @ symbols, then source the configuration
    grep -v "^[[:space:]]*[#@]" /etc/mesh-network/mesh-config.conf > /tmp/mesh-config-filtered.conf
    . /tmp/mesh-config-filtered.conf
    rm /tmp/mesh-config-filtered.conf
fi

LAN_BRIDGE="${LAN_BRIDGE:-br-lan}"
MESH_FORWARD_CHAIN="MESH-NETWORK-FWD"
MESH_NAT_CHAIN="MESH-NETWORK-NAT"

cleanup_mesh_firewall_chains() {
    if iptables -C FORWARD -j "${MESH_FORWARD_CHAIN}" >/dev/null 2>&1; then
        iptables -D FORWARD -j "${MESH_FORWARD_CHAIN}" 2>/dev/null || true
    fi
    iptables -F "${MESH_FORWARD_CHAIN}" 2>/dev/null || true
    iptables -X "${MESH_FORWARD_CHAIN}" 2>/dev/null || true

    if iptables -t nat -C POSTROUTING -j "${MESH_NAT_CHAIN}" >/dev/null 2>&1; then
        iptables -t nat -D POSTROUTING -j "${MESH_NAT_CHAIN}" 2>/dev/null || true
    fi
    iptables -t nat -F "${MESH_NAT_CHAIN}" 2>/dev/null || true
    iptables -t nat -X "${MESH_NAT_CHAIN}" 2>/dev/null || true
}

# Set up logging shared with mesh-network.sh, fallback to journald if necessary
LOG_DIR="/var/log/mesh-network"
LOG_FILE="${LOG_DIR}/mesh-network.log"
LOG_REDIRECTED=false

if mkdir -p "${LOG_DIR}" 2>/dev/null && touch "${LOG_FILE}" 2>/dev/null; then
    chmod 755 "${LOG_DIR}" 2>/dev/null || true
    chmod 644 "${LOG_FILE}" 2>/dev/null || true
    exec 1>>"${LOG_FILE}"
    exec 2>>"${LOG_FILE}"
    LOG_REDIRECTED=true
fi

if [ "${LOG_REDIRECTED}" != true ]; then
    if command -v systemd-cat >/dev/null 2>&1; then
        exec 1> >(systemd-cat -t mesh-network-stop)
        exec 2>&1
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] STOP: Falling back to journald logging" >&2
    else
        exec 1>>/tmp/mesh-network-stop.log
        exec 2>&1
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] STOP: Logging to /tmp/mesh-network-stop.log due to permission issues" >&2
    fi
fi

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] STOP: $1"
}

log "Stopping mesh network"

# Kill any existing monitoring processes
if [ -f /var/run/mesh-network-monitor.pid ]; then
    kill $(cat /var/run/mesh-network-monitor.pid) 2>/dev/null || true
    rm -f /var/run/mesh-network-monitor.pid
fi

# Flush all routes and addresses from bat0 interface
if ip link show bat0 >/dev/null 2>&1; then
    log "Cleaning up bat0 interface"
    ip route flush dev bat0 2>/dev/null || true
    ip addr flush dev bat0 2>/dev/null || true
    ip link set down dev bat0 2>/dev/null || true
fi

# Find and clean up mesh interface if MESH_IFACE is not set
if [ -z "${MESH_IFACE}" ]; then
    MESH_IFACE=$(batctl if 2>/dev/null | head -n1 | cut -f1) || true
fi

# Remove interface from batman-adv
if [ -n "${MESH_IFACE}" ]; then
    log "Removing ${MESH_IFACE} from batman-adv"
    batctl if del "${MESH_IFACE}" 2>/dev/null || true
fi

# Reset wireless interface
if [ -n "${MESH_IFACE}" ]; then
    log "Resetting ${MESH_IFACE}"
    ip link set down dev "${MESH_IFACE}" 2>/dev/null || true
    iwconfig "${MESH_IFACE}" mode managed 2>/dev/null || true
    ip addr flush dev "${MESH_IFACE}" 2>/dev/null || true
fi

# Tear down LAN bridge if it exists
if ip link show "${LAN_BRIDGE}" >/dev/null 2>&1; then
    log "Tearing down LAN bridge ${LAN_BRIDGE}"
    for iface in "${WAP_IFACE}" "${ETH_LAN}"; do
        if [ -n "${iface}" ] && ip link show "${iface}" >/dev/null 2>&1; then
            ip link set "${iface}" nomaster 2>/dev/null || true
        fi
    done
    ip link set "${LAN_BRIDGE}" down 2>/dev/null || true
    ip addr flush dev "${LAN_BRIDGE}" 2>/dev/null || true
    ip link delete "${LAN_BRIDGE}" type bridge 2>/dev/null || true
fi

# Unload batman-adv module if no other interfaces are using it
if ! batctl if 2>/dev/null | grep -q .; then
    log "Unloading batman-adv module"
    rmmod batman-adv 2>/dev/null || true
fi

# Remove mesh firewall rules
log "Cleaning up mesh firewall chains"
cleanup_mesh_firewall_chains

# Disable IP forwarding
sysctl -w net.ipv4.ip_forward=0 2>/dev/null || true

# Re-enable NetworkManager for the interface
if [ -n "${MESH_IFACE}" ]; then
    log "Re-enabling NetworkManager for ${MESH_IFACE}"
    nmcli device set "${MESH_IFACE}" managed yes 2>/dev/null || true
fi

log "Mesh network stopped successfully"
exit 0
