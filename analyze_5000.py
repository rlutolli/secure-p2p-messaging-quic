import json

def parse(file_path):
    try:
        with open(file_path, 'r') as f:
            data = json.load(f)
    except FileNotFoundError:
        print(f"File not found: {file_path}")
        return

    packets = []
    
    for packet in data:
        try:
            layers = packet['_source']['layers']
            frame = layers.get('frame', {})
            ip_layer = layers.get('ip', layers.get('ipv6', {}))
            
            src_ip = ip_layer.get('ip.src', ip_layer.get('ipv6.src', ''))
            if isinstance(src_ip, list): src_ip = src_ip[0]
            
            if not src_ip:
                continue
                
            frame_len = frame.get('frame.len', '0')
            if isinstance(frame_len, list): frame_len = frame_len[0]
            frame_len = int(frame_len)
            
            time_rel = frame.get('frame.time_relative', '0')
            if isinstance(time_rel, list): time_rel = time_rel[0]
            time_rel = float(time_rel)
            
            # The 5000 byte payloads are fragmented. Most packets are ~1300-1500 bytes.
            # TCP might also pack smaller pieces depending on MTU or ACKs.
            # We filter for large data-carrying packets > 1000 bytes.
            if frame_len > 1000:
                d = "C->S" if src_ip.endswith(".1") else "S->C"
                packets.append({'dir': d, 'rel_time': time_rel, 'len': frame_len})
                
        except Exception:
            continue
            
    print(f"\n--- {file_path.split('/')[-1]} ---")
    print(f"Total large packets (>1000 bytes): {len(packets)}")
    
    rtts = []
    
    i = 0
    while i < len(packets) - 1:
        if packets[i]['dir'] == 'C->S':
            j = i + 1
            # Skip any consecutive C->S packets
            while j < len(packets) and packets[j]['dir'] == 'C->S':
                j += 1
            if j < len(packets) and packets[j]['dir'] == 'S->C':
                rtt_ms = (packets[j]['rel_time'] - packets[i]['rel_time']) * 1000
                if rtt_ms < 100: # filter anomalies (e.g. miss-matched responses)
                    rtts.append(rtt_ms)
                i = j + 1
                continue
        i += 1
        
    if rtts:
        print(f"Paired Data Flights: {len(rtts)}")
        print(f"Avg Time-to-First-Response: {sum(rtts)/len(rtts):.3f} ms")
        print(f"Min TTFB: {min(rtts):.3f} ms")
        print(f"Max TTFB: {max(rtts):.3f} ms")
    else:
        print("No paired RTTs found.")

parse('/Users/rlutolli/Desktop/tcp_5000.json')
parse('/Users/rlutolli/Desktop/quic_5000.json')
