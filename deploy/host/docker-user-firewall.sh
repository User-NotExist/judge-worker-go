#!/bin/sh
# Example DOCKER-USER rules (§17.5). Docker bypasses ufw/firewalld for published ports,
# so restrict metrics ports and egress here. Adjust the CIDRs/IPs, then persist them with
# your config management (iptables-persistent, nftables include, …).
set -eu
MGMT_NET=${MGMT_NET:-10.20.0.0/24}           # Prometheus / operators
ALLOW_OUT=${ALLOW_OUT:-"10.10.0.10 10.10.0.11 10.10.0.12 10.10.0.20"}  # API, Redis, RustFS, registry
NTP=${NTP:-10.10.0.30}

iptables -F DOCKER-USER
iptables -A DOCKER-USER -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
# Inbound to published metrics ports: management network only.
iptables -A DOCKER-USER -p tcp -m multiport --dports 9100,5052 -s "$MGMT_NET" -j RETURN
iptables -A DOCKER-USER -p tcp -m multiport --dports 9100,5052 -j DROP
# Egress from containers: allow-list only.
for ip in $ALLOW_OUT; do iptables -A DOCKER-USER -d "$ip" -j RETURN; done
iptables -A DOCKER-USER -p udp --dport 123 -d "$NTP" -j RETURN
iptables -A DOCKER-USER -p udp --dport 53 -j RETURN      # internal DNS, tighten if possible
iptables -A DOCKER-USER -i docker0 -j DROP
iptables -A DOCKER-USER -i br+ -j DROP
iptables -A DOCKER-USER -j RETURN
