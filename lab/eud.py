#!/usr/bin/env python3
"""A minimal end-user device for the lab: DHCP over raw sockets (no client
binary needed) and plain DNS queries. Run inside the EUD's namespace.

  eud.py dhcp IFACE              DISCOVER/OFFER/REQUEST/ACK; prints key=value lines
  eud.py dhcp IFACE --rebind IP  INIT-REBOOT request for IP; prints ack or nak
  eud.py dns SERVER NAME         A query; prints the address
  eud.py pinger TARGET SECONDS   echo every 100 ms; prints reply/error lines with times
  eud.py renew IFACE IP SERVER   RENEWING: unicast REQUEST from IP:68 to SERVER:67;
                                 prints "ack ip=<addr> server=<id>", "nak" or "no reply"
  eud.py mlisten IFACE GROUP PORT SECONDS
                                 join a multicast group; prints "received <n>"
  eud.py msend IFACE GROUP PORT COUNT [SIZE]
                                 send COUNT datagrams (SIZE bytes, default 200)
                                 to the group, 20 ms apart
  eud.py igmpquery IFACE         send one IGMPv2 general query (a rogue querier)
  eud.py tcpserve ADDR PORT      answer each line with the client's source address
  eud.py tcpclient HOST PORT SECONDS
                                 one connection, one exchange every 250 ms for
                                 SECONDS (0: just one); prints "via <addr> <time>"
                                 per answer, "error <time> <why>" and exits 1 on failure
"""
import os
import random
import socket
import struct
import sys
import time

BCAST = b"\xff" * 6


def mac_of(iface):
    with open(f"/sys/class/net/{iface}/address") as f:
        return bytes.fromhex(f.read().strip().replace(":", ""))


def csum(data):
    if len(data) % 2:
        data += b"\0"
    s = sum(struct.unpack("!%dH" % (len(data) // 2), data))
    s = (s >> 16) + (s & 0xFFFF)
    s += s >> 16
    return (~s) & 0xFFFF


def dhcp_packet(mac, xid, msgtype, opts):
    body = struct.pack("!BBBBIHH", 1, 1, 6, 0, xid, 0, 0x8000)
    body += b"\0" * 16 + mac + b"\0" * 10 + b"\0" * 192 + b"\x63\x82\x53\x63"
    body += bytes([53, 1, msgtype])
    body += bytes([61, 7, 1]) + mac
    body += bytes([55, 6, 1, 3, 6, 15, 51, 54])
    host = b"eud-" + mac[-2:].hex().encode()
    body += bytes([12, len(host)]) + host
    for code, val in opts:
        body += bytes([code, len(val)]) + val
    body += b"\xff"
    udp = struct.pack("!HHHH", 68, 67, 8 + len(body), 0) + body
    ip = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(udp), random.randrange(65536), 0, 64, 17, 0,
                     b"\0\0\0\0", b"\xff\xff\xff\xff")
    ip = ip[:10] + struct.pack("!H", csum(ip)) + ip[12:]
    return BCAST + mac + b"\x08\x00" + ip + udp


def parse_opts(payload):
    opts = {}
    i = 240
    while i < len(payload):
        code = payload[i]
        if code == 255:
            break
        if code == 0:
            i += 1
            continue
        ln = payload[i + 1]
        opts[code] = payload[i + 2:i + 2 + ln]
        i += 2 + ln
    return opts


def wait_reply(sock, xid, timeout=5.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        sock.settimeout(max(0.05, deadline - time.time()))
        try:
            frame = sock.recv(4096)
        except socket.timeout:
            break
        if len(frame) < 14 + 20 + 8 + 240 or frame[12:14] != b"\x08\x00":
            continue
        ihl = (frame[14] & 0x0F) * 4
        if frame[14 + 9] != 17:
            continue
        udp = frame[14 + ihl:]
        if struct.unpack("!H", udp[2:4])[0] != 68:
            continue
        payload = udp[8:]
        if struct.unpack("!I", payload[4:8])[0] != xid:
            continue
        return payload
    return None


def ip4(b):
    return ".".join(str(x) for x in b[:4])


def cmd_dhcp(iface, rebind=None):
    mac = mac_of(iface)
    sock = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0800))
    sock.bind((iface, 0))
    xid = random.randrange(1, 2**32)
    if rebind:
        sock.send(dhcp_packet(mac, xid, 3, [(50, socket.inet_aton(rebind))]))
        reply = wait_reply(sock, xid)
        if reply is None:
            print("no reply")
            return 1
        print("nak" if parse_opts(reply).get(53) == b"\x06" else "ack")
        return 0
    sock.send(dhcp_packet(mac, xid, 1, []))
    offer = wait_reply(sock, xid)
    if offer is None:
        print("no offer")
        return 1
    o = parse_opts(offer)
    if o.get(53) != b"\x02":
        print("unexpected message type", o.get(53))
        return 1
    yiaddr = offer[16:20]
    server = o.get(54, b"\0\0\0\0")
    sock.send(dhcp_packet(mac, xid, 3, [(50, yiaddr), (54, server)]))
    ack = wait_reply(sock, xid)
    if ack is None:
        print("no ack")
        return 1
    a = parse_opts(ack)
    if a.get(53) != b"\x05":
        print("nak")
        return 1
    print("ip=" + ip4(ack[16:20]))
    print("server=" + ip4(a.get(54, b"\0\0\0\0")))
    print("router=" + ip4(a.get(3, b"\0\0\0\0")))
    print("dns=" + ip4(a.get(6, b"\0\0\0\0")))
    print("mask=" + ip4(a.get(1, b"\0\0\0\0")))
    print("lease=" + str(struct.unpack("!I", a.get(51, b"\0\0\0\0"))[0]))
    print("domain=" + a.get(15, b"").decode(errors="replace").rstrip("\0"))
    return 0


def cmd_renew(iface, ip, server, timeout=5.0):
    """What a client does at T1: a REQUEST with ciaddr set, unicast from its
    address to the server that leased it, wherever the client now is."""
    mac = mac_of(iface)
    xid = random.randrange(1, 2**32)
    body = bytearray(dhcp_packet(mac, xid, 3, [])[14 + 28:])
    body[10:12] = b"\0\0"  # no broadcast flag: the server answers unicast
    body[12:16] = socket.inet_aton(ip)
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, iface.encode())
    s.bind((ip, 68))
    s.sendto(bytes(body), (server, 67))
    deadline = time.time() + timeout
    while time.time() < deadline:
        s.settimeout(max(0.05, deadline - time.time()))
        try:
            payload = s.recv(4096)
        except socket.timeout:
            break
        if len(payload) < 240 or struct.unpack("!I", payload[4:8])[0] != xid:
            continue
        o = parse_opts(payload)
        if o.get(53) == b"\x05":
            print(f"ack ip={ip4(payload[16:20])} server={ip4(o.get(54, bytes(4)))}")
            return 0
        if o.get(53) == b"\x06":
            print("nak")
            return 0
    print("no reply")
    return 1


def cmd_dns(server, name):
    q = struct.pack("!HHHHHH", random.randrange(65536), 0x0100, 1, 0, 0, 0)
    for label in name.rstrip(".").split("."):
        q += bytes([len(label)]) + label.encode()
    q += b"\0" + struct.pack("!HH", 1, 1)
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(3)
    t0 = time.time()
    s.sendto(q, (server, 53))
    try:
        r, _ = s.recvfrom(512)
    except socket.timeout:
        print("timeout")
        return 1
    ms = int((time.time() - t0) * 1000)
    rcode = r[3] & 0x0F
    ancount = struct.unpack("!H", r[6:8])[0]
    if rcode != 0 or ancount == 0:
        print(f"error rcode={rcode} ms={ms}")
        return 1
    i = 12
    while r[i] != 0:
        i += 1 + r[i]
    i += 5
    for _ in range(ancount):
        if r[i] & 0xC0 == 0xC0:
            i += 2
        else:
            while r[i] != 0:
                i += 1 + r[i]
            i += 1
        typ, _, _, rdlen = struct.unpack("!HHIH", r[i:i + 10])
        i += 10
        if typ == 1 and rdlen == 4:
            print(f"{ip4(r[i:i + 4])} ms={ms}")
            return 0
        i += rdlen
    print(f"no A record ms={ms}")
    return 1


def cmd_pinger(target, seconds, interval=0.1):
    """Echo to target every interval for seconds; print 'reply <time>' for
    each answer and 'error <time>' for each ICMP error. Unlike ping, it never
    gives up on errors, so an outage can be timed from both ends."""
    import select
    s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP)
    ident = os.getpid() & 0xFFFF
    end = time.time() + float(seconds)
    seq = 0
    next_send = time.time()
    while time.time() < end:
        now = time.time()
        if now >= next_send:
            seq = (seq + 1) & 0xFFFF
            hdr = struct.pack("!BBHHH", 8, 0, 0, ident, seq) + b"trellis-lab-pinger"
            hdr = hdr[:2] + struct.pack("!H", csum(hdr)) + hdr[4:]
            try:
                s.sendto(hdr, (target, 0))
            except OSError:
                print(f"error {time.time():.6f} send", flush=True)
            next_send += interval
        r, _, _ = select.select([s], [], [], max(0.0, min(next_send, end) - time.time()))
        if not r:
            continue
        pkt, src = s.recvfrom(2048)
        ihl = (pkt[0] & 0x0F) * 4
        typ = pkt[ihl]
        try:
            if typ == 0 and struct.unpack("!H", pkt[ihl + 4:ihl + 6])[0] == ident:
                print(f"reply {time.time():.6f}", flush=True)
            elif typ in (3, 11):
                print(f"error {time.time():.6f} type={typ} code={pkt[ihl + 1]} from={src[0]}", flush=True)
        except BrokenPipeError:
            return 0
    return 0


def ifindex(iface):
    with open(f"/sys/class/net/{iface}/ifindex") as f:
        return int(f.read())


def cmd_mlisten(iface, group, port, seconds):
    """A phone running ATAK: joins the group (the kernel sends the IGMP
    report) and counts what arrives."""
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind((group, int(port)))
    mreq = struct.pack("4s4si", socket.inet_aton(group), socket.inet_aton("0.0.0.0"), ifindex(iface))
    s.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, mreq)
    print("joined", flush=True)
    n = 0
    end = time.time() + float(seconds)
    while time.time() < end:
        s.settimeout(max(0.05, end - time.time()))
        try:
            s.recv(2048)
            n += 1
        except socket.timeout:
            break
    print(f"received {n}", flush=True)
    return 0


def cmd_msend(iface, group, port, count, size=200):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF, struct.pack("4s4si", socket.inet_aton("0.0.0.0"), socket.inet_aton("0.0.0.0"), ifindex(iface)))
    s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_TTL, 1)
    s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_LOOP, 0)
    head = b"<event version='2.0' type='a-f-G-U-C' how='m-g'/>"
    payload = head + b"x" * max(0, int(size) - len(head))
    for _ in range(int(count)):
        s.sendto(payload, (group, int(port)))
        time.sleep(0.02)
    return 0


def cmd_igmpquery(iface):
    s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_IGMP)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, iface.encode())
    s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_TTL, 1)
    msg = struct.pack("!BBH4s", 0x11, 100, 0, b"\0\0\0\0")
    msg = msg[:2] + struct.pack("!H", csum(msg)) + msg[4:]
    s.sendto(msg, ("224.0.0.1", 0))
    return 0


def cmd_tcpserve(addr, port):
    """The lab internet's server: each line gets back the source address
    the server saw, so a client learns which gateway NATed its connection."""
    import threading
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((addr, int(port)))
    srv.listen(16)

    def serve(conn, peer):
        with conn:
            f = conn.makefile("rb")
            for _ in f:
                try:
                    conn.sendall(f"{peer[0]}\n".encode())
                except OSError:
                    return

    while True:
        conn, peer = srv.accept()
        threading.Thread(target=serve, args=(conn, peer), daemon=True).start()


def cmd_tcpclient(host, port, seconds, interval=0.25):
    end = time.time() + float(seconds)
    try:
        s = socket.create_connection((host, int(port)), timeout=3)
        f = s.makefile("rb")
        while True:
            s.sendall(b"x\n")
            line = f.readline()
            if not line:
                raise OSError("connection closed")
            print(f"via {line.decode().strip()} {time.time():.6f}", flush=True)
            if time.time() >= end:
                return 0
            time.sleep(interval)
    except OSError as e:
        print(f"error {time.time():.6f} {e}", flush=True)
        return 1


def main(argv):
    if len(argv) >= 2 and argv[0] == "dhcp":
        rebind = argv[3] if len(argv) >= 4 and argv[2] == "--rebind" else None
        return cmd_dhcp(argv[1], rebind)
    if len(argv) == 3 and argv[0] == "pinger":
        return cmd_pinger(argv[1], argv[2])
    if len(argv) == 5 and argv[0] == "mlisten":
        return cmd_mlisten(*argv[1:])
    if len(argv) in (5, 6) and argv[0] == "msend":
        return cmd_msend(*argv[1:])
    if len(argv) == 2 and argv[0] == "igmpquery":
        return cmd_igmpquery(argv[1])
    if len(argv) == 4 and argv[0] == "renew":
        return cmd_renew(argv[1], argv[2], argv[3])
    if len(argv) == 3 and argv[0] == "tcpserve":
        return cmd_tcpserve(argv[1], argv[2])
    if len(argv) == 4 and argv[0] == "tcpclient":
        return cmd_tcpclient(argv[1], argv[2], argv[3])
    if len(argv) == 3 and argv[0] == "dns":
        return cmd_dns(argv[1], argv[2])
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
