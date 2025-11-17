# Mobile Ad-hoc Network (MANET) Deployment Suite

## Table of Contents
- [Introduction](#introduction)
- [What is a MANET?](#what-is-a-manet)
- [Requirements](#requirements)
- [Installation](#installation)
- [Configuration](#configuration)
- [Service Operation](#service-operation)
- [Troubleshooting](#troubleshooting)
- [Security Considerations](#security-considerations)
- [Monitoring and Maintenance](#monitoring-and-maintenance)
- [References](#references)

## Introduction

The MANET Deployment Suite is a comprehensive toolkit for deploying resilient, self-healing wireless mesh networks using B.A.T.M.A.N. advanced (batman-adv) - a layer 2 routing protocol implemented as a Linux kernel module specifically designed for mobile ad-hoc networks.

This suite simplifies the deployment process with:

- **Simplified IP networking** - Maintains layer 3 connectivity over batman-adv mesh after initial configuration
- **Intelligent gateway management** - Continuously discovers, validates, and selects optimal internet gateways
- **Seamless failover** - Maintains network connectivity during gateway or node failures
- **Flexible deployment options** - Supports various topologies from simple meshes to complex multi-gateway configurations
- **Real-time adaptation** - Responds to changing network conditions by updating routing tables automatically
- **Enhanced node tracking** - Persistent translation table for reliable node identification and addressing

Once configured and installed, the `mesh-network.service` handles the tasks of managing gateway selection, routing configuration, and network monitoring with minimal intervention, ensuring maximum network availability.

The system seamlessly bridges meshes with external networks, enabling internet access across the entire mesh and integration with existing infrastructure like TAK servers or other communication systems.

### Key Features
- Maintain an IP layer on batman-adv with minimal ongoing configuration
- Automatic gateway detection and selection after initial setup
- Seamless failover between gateways
- Support for multiple network topologies
- Integrated monitoring and maintenance tools
- Persistent node tracking with translation table functionality

## What is a MANET?

A Mobile Ad-hoc Network (MANET) is a decentralized type of wireless network that doesn't rely on pre-existing infrastructure. Each node participates in routing by forwarding data to other nodes, and the determination of which nodes forward data is made dynamically based on network connectivity.

Key characteristics:
- Self-forming and self-healing
- No central infrastructure required
- Dynamic routing based on network conditions
- Peer-to-peer communication between nodes

Common applications include:
- Emergency response and disaster recovery
- Military tactical networks
- Underground mining operations
- Construction site communications
- Temporary event networks
- Remote area connectivity

## Requirements

### Hardware Requirements
- **Minimum**:
  - Single-board computer (Raspberry Pi, Libre Computer, etc.)
  - WiFi adapter that supports ad-hoc mode (check with "iw list" look for IBSS mode)
  - Power supply
  - SD card/storage (eMMC is highly recommended over an SD card)

### Software Requirements
- This tool is optimized for Ubuntu 22.04. Other Debian-based distributions are supported, but variations in their default network management tools might necessitate configuration adjustments or disabling specific components.
- Required packages (also listed in `requirements/packages.txt`). The `setup.sh` script installs or updates everything on that list automatically, but you can install them manually with:
  ```bash
  sudo apt install batctl iw wireless-tools network-manager net-tools bridge-utils iptables dnsmasq hostapd arping arp-scan bc
  ```
  
## Installation

### System Preparation
```bash
# Update system
sudo apt update && sudo apt upgrade -y

# Install git (setup.sh installs all mesh dependencies automatically)
sudo apt install -y git

# Clone repository
git clone https://github.com/ifHoncho/manet-deployment-suite.git
cd manet-deployment-suite

# Initial Setup
sudo chmod +x setup.sh
sudo ./setup.sh

# Edit configuration
sudo nano /etc/mesh-network/mesh-config.conf

# Enable and start service
sudo systemctl enable mesh-network.service
sudo systemctl start mesh-network.service

# Optional: view logs to see what's happening
sudo journalctl -u mesh-network.service -f
# A copy is also written to /var/log/mesh-network/mesh-network.log
tail -f /var/log/mesh-network/mesh-network.log
```

## Configuration

### Basic Configuration
Minimum required settings in `/etc/mesh-network/mesh-config.conf`
Most of these can be left as is, the main things you will need to change are the NODE_IP, since each node needs a unique IP address. And the MESH_IFACE which needs to match whatever WiFi interface you're using for the mesh. More details are commented in the actual config file.

```bash
# Interface Configuration
MESH_IFACE=wlan0          # Primary mesh interface

# IP configuration - These must be unique on the network, each node should have a different IP address.
# Remember to configure IP's within the netmask (eg. for /24, you can only increment the last octet)
# It is helpful to create an /etc/bat-hosts file with numerical hostnames for each node so you know each IP addresses without having to look it up. (Given that you assign IP's in numerical order)
NODE_IP=10.0.0.1          # Node IP (must be unique on the network)
MESH_NETMASK=24           # Network mask (e.g., 24 for /24)
DNS_SERVERS=9.9.9.9,8.8.8.8,1.1.1.1  # DNS servers to use

# Mesh Parameters
MESH_MODE=ad-hoc          # Must be set to ad-hoc
MESH_ESSID=mesh-network   # Network name (same for all nodes)
MESH_CHANNEL=1            # WiFi channel (1-13, depending on region)
MESH_CELL_ID=02:12:34:56:78:9A  # Cell ID (same for all nodes)

# Batman-adv Settings
```

### Advanced Configuration
Optional settings for enhanced functionality:

```bash
# Additional Interface Options
WAP_IFACE=wlan1           # Access point interface for client devices
WAP_SSID=MeshAccess01     # WiFi access point SSID
WAP_PASSWORD=meshpassword # WiFi access point password
WAP_CHANNEL=6             # WiFi access point channel
WAP_HW_MODE=g             # Hardware mode - g for 2.4GHz, a for 5GHz
ETH_WAN=eth0              # Ethernet WAN interface (internet connection)
ETH_LAN=eth1              # Ethernet LAN interface

# Additional IP Configuration
WAP_IP=10.10.0.1          # IP for wireless access point interface
ETH_LAN_IP=10.10.0.2      # IP for ethernet LAN interface

# Performance Tuning
MESH_MTU=1500             # MTU size for mesh interface
BATMAN_ORIG_INTERVAL=1000 # Originator interval (ms)
BATMAN_HOP_PENALTY=30     # Hop penalty
BATMAN_ROUTING_ALGORITHM=BATMAN_V  # BATMAN_IV or BATMAN_V Note that BATMAN_V is only supported in newer versions of batctl
```

If you leave `WAP_PASSWORD` blank (or shorter than 8 characters), the service generates a strong random password automatically. The current value is written to `/var/lib/mesh-network/generated-wap-password`, which is readable only by root.

### Hardware Interface Management

If you have issues with interface names changing after a reboot, you can create a file in /etc/systemd/network/ and add from the template below.
Note that this might cause issues if you try to boot a different interface connected or if you have another tool managing interface names. This will be obvious if the system hangs at boot.

```bash
# Create a file like /etc/systemd/network/10-wlan0.link:
# This is an example to set a given interface to wlan0, you can name it whatever though. Just change the filename and Name value under [link]
[Match]
MACAddress=xx:xx:xx:xx:xx:xx
[Link]
Name=wlan0
```

### Network Planning
For optimal performance, consider:
1. Each node should have a unique IP address (e.g., follow pattern 10.0.0.x for nodes (depending on netmask))
2. Creating an `/etc/bat-hosts` file with numerical hostnames for easy reference
3. Access point and LAN networks can use separate subnets (e.g., 10.x0.0.x)

## Service Operation

The mesh network is managed through systemd:

```bash
# Enable auto-start
sudo systemctl enable mesh-network.service

# Start the service
sudo systemctl start mesh-network.service

# Check status
sudo systemctl status mesh-network.service
```

### Service Logging
The service maintains detailed logs in multiple locations:

1. **Main Service Log**:
   ```bash
   # View mesh network service log
   tail -f /var/log/mesh-network/mesh-network.log
   
   # View last 100 lines with timestamps
   tail -n 100 /var/log/mesh-network/mesh-network.log
   
   # Search for specific events
   grep "gateway" /var/log/mesh-network/mesh-network.log
   grep "error" /var/log/mesh-network/mesh-network.log
   ```

2. **Systemd Journal**:
   ```bash
   # Follow service logs in real-time
   sudo journalctl -u mesh-network.service -f
   
   # View logs since last boot
   sudo journalctl -u mesh-network.service -b
   
   # View logs with timestamps
   sudo journalctl -u mesh-network.service --output=short-precise
   ```

3. **System Logs**:
   ```bash
   # Check kernel messages related to batman-adv
   dmesg | grep batman
   
   # View system logs for network-related events
   tail -f /var/log/syslog | grep -E "batman|mesh|wlan"
   ```

### Log Analysis
Important log entries to watch for:

1. **Gateway Detection**:
   ```
   [timestamp] Starting gateway detection
   [timestamp] Found batman-adv gateway MAC: xx:xx:xx:xx:xx:xx
   [timestamp] Verified x.x.x.x is a batman-adv gateway
   ```

2. **Route Configuration**:
   ```
   [timestamp] Configuring routing for gateway x.x.x.x
   [timestamp] Successfully added default route via x.x.x.x
   ```

3. **Error Conditions**:
   ```
   [timestamp] Failed to add default route
   [timestamp] Gateway unreachable after multiple attempts
   [timestamp] Interface not ready or down
   ```

## Troubleshooting

### Common Issues

1. **Service Fails to Start**
   ```bash
   # Check service status
   sudo systemctl status mesh-network.service
   
   # View detailed logs
   sudo journalctl -u mesh-network.service -f
   tail -f /var/log/mesh-network/mesh-network.log
   
   # Check for errors in system log
   grep -i error /var/log/mesh-network/mesh-network.log
   ```

2. **No Gateway Connection**
   ```bash
   # Check batman-adv gateway list
   sudo batctl gwl
   
   # Verify ARP entries
   arp -a | grep bat0
   
   # Test gateway ping
   sudo batctl ping <gateway-ip>
   
   # Check gateway detection logs
   tail -f /var/log/mesh-network/mesh-network.log | grep -E "gateway|route"
   
   # Check translation table entries
   cat /var/lib/batman-adv/translation_table.db
   ```

3. **Interface Problems**
   ```bash
   # Check interface status
   ip link show
   iwconfig
   
   # Verify batman-adv is loaded
   lsmod | grep batman
   
   # Check batman-adv interface
   batctl if
   
   # Monitor interface-related logs
   tail -f /var/log/mesh-network/mesh-network.log | grep -E "interface|wlan"
   ```

### Diagnostic Commands
```bash
# Real-time log monitoring
tail -f /var/log/mesh-network/mesh-network.log

# Check batman-adv status
sudo batctl o         # Originator table
sudo batctl t        # Translation table
sudo batctl d        # Debug log

# Network diagnostics
sudo iw dev          # Wireless interface info
sudo iwconfig        # Wireless settings
sudo iftop -i bat0   # Network traffic

# System checks
dmesg | grep batman  # Kernel messages
sudo sysctl -a | grep batman  # Kernel parameters

# Translation table management
cat /var/lib/batman-adv/translation_table.db  # View current table entries
```

### Performance Monitoring
Monitor network performance using various tools:

1. **Traffic Analysis**:
   ```bash
   # Monitor interface traffic
   sudo iftop -i bat0
   
   # View bandwidth usage
   sudo nethogs bat0
   
   # Check interface statistics
   cat /sys/class/net/bat0/statistics/*
   ```

2. **Gateway Status**:
   ```bash
   # Check current gateway
   ip route show | grep default
   
   # Monitor gateway changes
   tail -f /var/log/mesh-network/mesh-network.log | grep "gateway"
   
   # View gateway list
   sudo batctl gwl
   ```

3. **Node Status**:
   ```bash
   # View originator table
   sudo batctl o
   
   # View translation table (IP to MAC mappings)
   cat /var/lib/batman-adv/translation_table.db
   ```

## Security Considerations

It is important to note that <u>this is not a secure implementation.</u> This project is still in its infancy, speed of development and ease of install has been prioritized over ground up security. At its default state, it relies solely on obscurity. You need to assume that every node on your mesh could be malicious and that all traffic will be clearly visible to anyone in proximity who wants to view it.

For deployments handling sensitive data, I recommend implementing a Peer-to-Peer (P2P) VPN overlay on top of the batman-adv mesh. tincVPN seems to be the best option for this. I'm currently testing and refining this approach, future releases will eventually include a streamlined implementation of this process, but for now, it will require you to get your hands dirty. You'll need to manually configure each node with its own cryptographic key pair, share public keys securely between peers, and ensure that Tinc is only communicating over the BATMAN-adv interface. This setup will encrypt all inter-node traffic at the VPN layer, preventing eavesdropping or tampering by untrusted nodes within RF range or on the mesh.

Keep in mind that Tinc's default behavior still requires some level of trust between peers, as it doesn't include a full public key infrastructure (PKI) or revocation mechanism out of the box. To mitigate this, maintain strict control over which keys are accepted and monitor the mesh for rogue peers. You can automate this process with scripts or configuration management tools, but it's important to understand the trust relationships you're establishing. This goes a bit outside the scope of what I'm aiming to do with this project right now, but it will slowly develop over time as I experiment and learn more about an optimal way to integrate it into this project.

Ideally, future versions of this project will aim to include:

    Encrypted and authenticated peer discovery

    Automatic key distribution and rotation

    Optional integration with distributed identity or trust frameworks

Until then, you'll need to implement this yourself.

## Monitoring and Maintenance

### Log Monitoring
1. **Viewing Logs**:
   ```bash
   # Check logs
   tail /var/log/mesh-network/mesh-network.log
   
   # Monitor specific events
   tail -f /var/log/mesh-network/mesh-network.log | grep --color=auto -E 'error|warning|gateway|route'
   ```

2. **Log Rotation**:
   The service automatically rotates logs to prevent disk space issues. Old logs are stored with timestamps in the subdirectory:
   ```
   /var/log/mesh-network/old/mesh-network.log.YYYYMMDD-HHMMSS
   ```

3. **Translation Table**:
   The system maintains a persistent translation table to map IP addresses to batman-adv MAC addresses:
   ```bash
   # View the current translation table
   cat /var/lib/batman-adv/translation_table.db
   
   # Format: timestamp|ip|bat0_mac|hw_mac
   # Entries older than 1 hour are automatically cleaned
   ```

4. **Log Analysis Tools**:
   ```bash
   # Count occurrences of specific events
   grep -c "gateway" /var/log/mesh-network/mesh-network.log
   
   # View unique gateway IPs
   grep "gateway" /var/log/mesh-network/mesh-network.log | grep -oE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' | sort -u
   
   # Check for common errors
   grep -i error /var/log/mesh-network/mesh-network.log | sort | uniq -c
   ```

## References

- [Batman-adv Documentation](https://www.open-mesh.org/projects/batman-adv/wiki)
- [Linux Wireless](https://wireless.wiki.kernel.org/)
- [Netfilter/iptables](https://netfilter.org/documentation/)
