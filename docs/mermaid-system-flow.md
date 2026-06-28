# Secure P2P Messaging — System Flow

> A beautiful visual guide to how your decentralized chat app works.
> Render this in any Mermaid viewer (GitHub, Notion, VS Code, etc.)

```mermaid
%%{init: {'theme': 'base', 'themeVariables': { 'fontSize': '16px', 'fontFamily': 'Inter, sans-serif' }}}%%
flowchart TB
    %% ===== Nodes =====
    USER([👤 You]):::user
    CLI[💻 CLI App]:::core
    ROOM{🚪 Room}:::core
    CREATE[✨ Create Room]:::action
    JOIN[🔎 Join Room]:::action
    DISC[📡 Discovery]:::network
    LAN{{🔊 LAN Broadcast}}:::network
    WAN{{🌐 Rendezvous}}:::network
    CONN[🔗 Connection Manager]:::core
    SEC[🔐 TLS 1.3 + TOFU Pinning]:::security
    SERVER[📥 Message Server]:::core
    MSG[💬 Send Message]:::action
    BROADCAST[📨 Broadcast to Peers]:::network
    HEALTH[💓 Health Check]:::support
    PING["PING / PONG"]:::support
    RELAY[(⭐ Relay Mode)]:::special

    %% ===== Flow =====
    USER --> CLI
    CLI --> ROOM
    ROOM --> CREATE
    ROOM --> JOIN

    CREATE --> SERVER
    CREATE --> DISC
    JOIN --> DISC

    DISC --> LAN
    DISC --> WAN
    LAN --> CONN
    WAN --> CONN

    CONN --> SEC
    SEC -->|Trust on First Use| CONN
    CONN -->|Opens Stream| SERVER
    SERVER -->|Accepts| CONN

    USER --> MSG
    MSG --> SERVER
    SERVER --> BROADCAST
    BROADCAST --> CONN
    CONN -->|Receive| USER

    SERVER -.-> RELAY
    RELAY -.-> BROADCAST

    CONN -.-> HEALTH
    HEALTH -.-> PING
    PING -.-> CONN

    %% ===== Subgraphs =====
    subgraph 🏠 "Your Machine"
        direction TB
        USER
        CLI
        ROOM
        CREATE
        JOIN
        MSG
    end

    subgraph 🌎 "The Network"
        direction TB
        LAN
        WAN
        BROADCAST
    end

    subgraph 🛡️ "Secure Engine"
        direction TB
        DISC
        CONN
        SEC
        SERVER
        HEALTH
        PING
        RELAY
    end

    %% ===== Styling =====
    classDef user fill:#e3f2fd,stroke:#1976d2,stroke-width:3px,color:#0d47a1
    classDef core fill:#fff3e0,stroke:#f57c00,stroke-width:2px,color:#e65100
    classDef action fill:#e8f5e9,stroke:#388e3c,stroke-width:2px,color:#1b5e20
    classDef network fill:#f3e5f5,stroke:#7b1fa2,stroke-width:2px,color:#4a148c
    classDef security fill:#ffebee,stroke:#c62828,stroke-width:2px,color:#b71c1c
    classDef support fill:#e0f7fa,stroke:#00838f,stroke-width:2px,color:#006064
    classDef special fill:#fffde7,stroke:#f9a825,stroke-width:2px,color:#f57f17

    %% Link styling
    linkStyle default stroke:#78909c,stroke-width:2px
    linkStyle 10,11,12,13,14,15 stroke:#7b1fa2,stroke-width:2px
    linkStyle 16,17,18 stroke:#c62828,stroke-width:2px,stroke-dasharray: 5 5
```

---

## 🎨 Color Legend

| Color | Meaning |
|-------|---------|
| 🔵 **Blue** | You — the user |
| 🟠 **Orange** | Core app components |
| 🟢 **Green** | Actions you take |
| 🟣 **Purple** | Network & discovery |
| 🔴 **Red** | Security & encryption |
| 🩵 **Cyan** | Background processes |
| 🟡 **Yellow** | Special modes (Relay) |

---

## 🚀 How It Works (In Plain English)

1. **You start the app** → Choose to create a new chat room or join an existing one.
2. **The app listens** → Your machine becomes a server, ready to accept connections.
3. **It finds peers** → Using LAN broadcast (local network) or a rendezvous server (internet), it discovers who's around.
4. **Secure handshake** → Every connection uses **TLS 1.3** with **Trust-On-First-Use** pinning — no central authority needed.
5. **Chat away** → Messages are broadcast to all connected peers in real-time.
6. **Stay healthy** → Automatic **PING/PONG** checks keep connections alive and clean up dead peers.
7. **Relay mode** *(optional)* → One peer can act as a star hub, letting everyone chat even across routers/firewalls.

---

*Generated for Secure P2P Messaging (QUIC) — v0.4*
