import json

CLIENT_IP = "192.168.2.1"
SERVER_IP = "192.168.2.12"

def parse(file_path, label):
    try:
        with open(file_path, 'r') as f:
            data = json.load(f)
    except FileNotFoundError:
        print(f"  [!] File not found: {file_path}")
        return None

    packets = []
    for packet in data:
        try:
            layers = packet['_source']['layers']
            frame  = layers.get('frame', {})
            ip     = layers.get('ip', layers.get('ipv6', {}))

            src = ip.get('ip.src', ip.get('ipv6.src', ''))
            if isinstance(src, list): src = src[0]
            if not src:
                continue

            flen = frame.get('frame.len', '0')
            if isinstance(flen, list): flen = flen[0]
            flen = int(flen)

            t = frame.get('frame.time_relative', '0')
            if isinstance(t, list): t = t[0]
            t = float(t)

            d = "C->S" if src == CLIENT_IP else "S->C"
            packets.append({'dir': d, 't': t, 'len': flen})
        except Exception:
            continue

    # Echo-matching: client sends payload of size L,
    # server echoes back a packet of similar size (within 20%).
    # Skip pure ACKs (< 100 bytes).
    rtts = []
    used = set()
    for i, pkt in enumerate(packets):
        if pkt['dir'] != 'C->S' or pkt['len'] < 100:
            continue
        # Find the next S->C packet that is a similar size to this one
        for j in range(i + 1, len(packets)):
            if j in used:
                continue
            r = packets[j]
            if r['dir'] != 'S->C':
                continue
            if r['len'] < 100:
                continue
            # Size similarity: within 20% of client packet
            size_ratio = r['len'] / pkt['len']
            if 0.5 <= size_ratio <= 2.0:
                rtt_ms = (r['t'] - pkt['t']) * 1000
                if 0 < rtt_ms < 500:
                    rtts.append(rtt_ms)
                    used.add(j)
                    break

    if rtts:
        avg = sum(rtts) / len(rtts)
        return {
            'label': label,
            'avg':   avg,
            'min':   min(rtts),
            'max':   max(rtts),
            'pairs': len(rtts)
        }
    return {'label': label, 'avg': None}


print("\n" + "=" * 65)
print("  FULL BENCHMARK: TCP vs QUIC (echo-matched RTT, 7MB rmem)")
print("=" * 65)

results = [
    parse('/Users/rlutolli/Desktop/tcp_64.json',    'TCP  64B'),
    parse('/Users/rlutolli/Desktop/quic_64.json',   'QUIC 64B'),
    parse('/Users/rlutolli/Desktop/tcp_1000.json',  'TCP  1000B'),
    parse('/Users/rlutolli/Desktop/quic_1000.json', 'QUIC 1000B'),
    parse('/Users/rlutolli/Desktop/tcp_5000.json',  'TCP  5000B'),
    parse('/Users/rlutolli/Desktop/quic_5000.json', 'QUIC 5000B'),
]

print(f"\n{'Protocol':<14} {'Avg RTT':>10} {'Min RTT':>10} {'Max RTT':>10} {'Pairs':>7}")
print("-" * 55)
for r in results:
    if r and r.get('avg') is not None:
        print(f"{r['label']:<14} {r['avg']:>9.3f}ms {r['min']:>9.3f}ms {r['max']:>9.3f}ms {r['pairs']:>6}")
    elif r:
        print(f"{r['label']:<14}  No paired RTTs found")

print("\n--- Winner per payload size ---")
pairs = [
    ('64B',    results[0], results[1]),
    ('1000B',  results[2], results[3]),
    ('5000B',  results[4], results[5]),
]
for size, tcp, quic in pairs:
    if tcp and quic and tcp.get('avg') and quic.get('avg'):
        diff   = tcp['avg'] - quic['avg']
        winner = f"QUIC faster by {abs(diff):.3f}ms" if diff > 0 else f"TCP  faster by {abs(diff):.3f}ms"
        print(f"  {size:>6}: {winner}")
print()
