#!/bin/bash

# Check if running as root
if [ "$EUID" -ne 0 ]; then 
    echo "Please run as root"
    exit 1
fi

# --- Check for required packages ---
SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REQUIREMENTS_FILE="${SCRIPT_DIR}/requirements/packages.txt"

if [ ! -f "${REQUIREMENTS_FILE}" ]; then
    echo "Error: requirements file not found at ${REQUIREMENTS_FILE}"
    exit 1
fi

REQUIRED_PACKAGES=()
while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
        ''|\#*)
            continue
            ;;
        *)
            REQUIRED_PACKAGES+=("$line")
            ;;
    esac
done < "${REQUIREMENTS_FILE}"

if [ ${#REQUIRED_PACKAGES[@]} -eq 0 ]; then
    echo "Error: requirements file ${REQUIREMENTS_FILE} does not list any packages."
    exit 1
fi

MISSING_PACKAGES=()

echo "Checking for required packages..."
for pkg in "${REQUIRED_PACKAGES[@]}"; do
    if ! dpkg -s "$pkg" > /dev/null 2>&1; then
        MISSING_PACKAGES+=("$pkg")
    fi
done

if [ ${#MISSING_PACKAGES[@]} -ne 0 ]; then
    echo "The following required packages are missing:"
    printf ' - %s\n' "${MISSING_PACKAGES[@]}"
    echo
    echo "Attempting to install missing packages..."
    export DEBIAN_FRONTEND=noninteractive
    if ! apt-get update; then
        echo "Error: Failed to update package lists via apt-get."
        exit 1
    fi
    if ! apt-get install -y "${MISSING_PACKAGES[@]}"; then
        echo "Error: Failed to install required packages: ${MISSING_PACKAGES[*]}"
        echo "If you get an error from dnsmasq saying 'Failed to start up' you can ignore it."
        exit 1
    fi
    echo "Successfully installed required packages."
else
    echo "All required packages are already installed."
fi
echo

# Check if rfkill is installed and unblock all devices
if command -v rfkill &> /dev/null; then
    echo "rfkill found, unblocking all devices..."
    rfkill unblock all
else
    echo "rfkill not found, skipping unblock step."
fi
echo

# --- Argument Parsing ---
SKIP_CONFIG_COPY=false
if [ "$1" == "nc" ] || [ "$1" == "noconfig" ]; then
    echo "Argument '$1' provided: Skipping config file copy."
    SKIP_CONFIG_COPY=true
fi

# Unmask hostapd service
echo "Unmasking hostapd.service..."
systemctl unmask hostapd > /dev/null 2>&1

# Disable dnsmasq and hostapd from starting automatically
echo "Disabling dnsmasq and hostapd from starting automatically..."
systemctl disable dnsmasq.service > /dev/null 2>&1
systemctl disable hostapd.service > /dev/null 2>&1

# Disable systemd-resolved
echo "Disabling systemd-resolved..."
systemctl stop systemd-resolved
systemctl disable systemd-resolved

# Enter custom DNS settings
echo "Entering custom DNS settings..."
rm /etc/resolv.conf
echo "nameserver 127.0.0.1" > /etc/resolv.conf
echo "nameserver 9.9.9.9" >> /etc/resolv.conf

# Add hostname to /etc/hosts
echo "Adding hostname to /etc/hosts..."
sed -i "2s/.*/127.0.1.1 $(hostname)/" /etc/hosts

# Create mesh-network directory
mkdir -p /etc/mesh-network

# Copy configuration files
echo "Copying service files..."
if [ "${SKIP_CONFIG_COPY}" = false ]; then
    # Backup existing config if it exists
    if [ -f "/etc/mesh-network/mesh-config.conf" ]; then
        echo "Backing up existing mesh config to /etc/mesh-network/mesh-config.conf.bak..."
        cp -f /etc/mesh-network/mesh-config.conf /etc/mesh-network/mesh-config.conf.bak || echo "Warning: Failed to create backup."
    fi
    echo "Copying mesh configuration file..."
    cp mesh_tools/mesh-config.conf /etc/mesh-network/

fi
cp mesh_tools/mesh-network.service /etc/systemd/system/
cp mesh_tools/mesh-network.sh /usr/sbin/
cp mesh_tools/mesh-network-stop.sh /usr/sbin/
cp mesh_tools/set-mac.sh /usr/sbin/

# Set permissions
echo "Setting permissions..."
chmod 644 /etc/systemd/system/mesh-network.service
chmod +x /usr/sbin/mesh-network.sh
chmod +x /usr/sbin/mesh-network-stop.sh
chmod +x /usr/sbin/set-mac.sh

# Reload systemd to recognize new service
systemctl daemon-reload > /dev/null 2>&1

echo "Setup complete. Files have been moved and permissions set."
echo
echo "You can now edit /etc/mesh-network/mesh-config.conf and start the service with:"
echo "systemctl enable mesh-network.service"
echo "systemctl start mesh-network.service"
echo

if command -v bf &> /dev/null; then
    bf misc/Y29tZWR5.bf
fi
echo