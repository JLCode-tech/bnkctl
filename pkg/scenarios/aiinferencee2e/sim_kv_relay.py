"""KV-event relay for llm-d-inference-sim (ZMTP 3.0, NULL security, Python stdlib only).

The simulator's PUB socket only connects out; vLLM binds and every endpoint picker connects
to each pod. The simulator connects to this relay on 127.0.0.1:RELAY_UPSTREAM_PORT; pickers
subscribe on 0.0.0.0:RELAY_PORT, as they would to vLLM's --kv-events-config endpoint.
"""
import os
import queue
import socket
import struct
import threading
import time

UPSTREAM_PORT = int(os.environ.get("RELAY_UPSTREAM_PORT", "5557"))
PORT = int(os.environ.get("RELAY_PORT", "20080"))
QUEUE_MAX = 10000  # messages buffered per subscriber before dropping, like a PUB high-water mark

subs = set()
subs_lock = threading.Lock()
stats = {"in": 0, "out": 0, "dropped": 0}


def log(msg):
    print(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), msg, flush=True)


def read_exact(conn, n):
    buf = bytearray()
    while len(buf) < n:
        chunk = conn.recv(n - len(buf))
        if not chunk:
            raise EOFError("peer closed")
        buf += chunk
    return bytes(buf)


def read_frame(conn):
    flags = read_exact(conn, 1)[0]
    size = struct.unpack(">Q", read_exact(conn, 8))[0] if flags & 0x02 else read_exact(conn, 1)[0]
    return flags, read_exact(conn, size)


def encode_frame(body, more=False, command=False):
    flags = (0x01 if more else 0) | (0x04 if command else 0)
    if len(body) > 255:
        return bytes([flags | 0x02]) + struct.pack(">Q", len(body)) + body
    return bytes([flags, len(body)]) + body


def read_message(conn):
    """Return (frames, None) for a message or (None, body) for a command."""
    frames = []
    while True:
        flags, body = read_frame(conn)
        if flags & 0x04:
            return None, body
        frames.append(body)
        if not flags & 0x01:
            return frames, None


def handshake(conn, socket_type):
    greeting = b"\xff" + b"\x00" * 8 + b"\x7f" + b"\x03\x00" + b"NULL".ljust(20, b"\x00") + b"\x00" * 32
    conn.sendall(greeting)
    peer = read_exact(conn, 64)
    if peer[0] != 0xFF or peer[9] != 0x7F or peer[10] < 3:
        raise ValueError("peer is not ZMTP 3")
    name = b"Socket-Type"
    ready = b"\x05READY" + bytes([len(name)]) + name + struct.pack(">I", len(socket_type)) + socket_type
    conn.sendall(encode_frame(ready, command=True))
    _, body = read_message(conn)
    if body is None or not body.startswith(b"\x05READY"):
        raise ValueError("expected READY")


def answer_command(conn, body, lock=None):
    """Reply to ZMTP 3.1 PING; ignore other commands."""
    if body.startswith(b"\x04PING"):
        pong = encode_frame(b"\x04PONG" + body[7:], command=True)
        if lock:
            with lock:
                conn.sendall(pong)
        else:
            conn.sendall(pong)


class Subscriber:
    def __init__(self, conn, addr):
        self.conn, self.addr = conn, addr
        self.topics = set()
        self.queue = queue.Queue(maxsize=QUEUE_MAX)
        self.send_lock = threading.Lock()
        self.closed = False

    def wants(self, topic):
        return any(topic.startswith(t) for t in list(self.topics))

    def offer(self, data):
        try:
            self.queue.put_nowait(data)
        except queue.Full:
            stats["dropped"] += 1

    def writer(self):
        try:
            while not self.closed:
                data = self.queue.get()
                if data is None:
                    break
                with self.send_lock:
                    self.conn.sendall(data)
                stats["out"] += 1
        except OSError:
            pass
        self.close()

    def reader(self):
        try:
            while True:
                frames, command = read_message(self.conn)
                if command is not None:
                    if command.startswith(b"\x09SUBSCRIBE"):
                        self.topics.add(command[10:])
                    elif command.startswith(b"\x06CANCEL"):
                        self.topics.discard(command[7:])
                    else:
                        answer_command(self.conn, command, self.send_lock)
                elif frames and frames[0][:1] == b"\x01":
                    self.topics.add(frames[0][1:])
                elif frames and frames[0][:1] == b"\x00":
                    self.topics.discard(frames[0][1:])
        except (OSError, EOFError, ValueError):
            pass
        self.close()

    def close(self):
        if self.closed:
            return
        self.closed = True
        with subs_lock:
            subs.discard(self)
        try:
            self.queue.put_nowait(None)  # wake the writer
        except queue.Full:
            pass
        try:
            self.conn.close()
        except OSError:
            pass
        log(f"subscriber {self.addr} disconnected")


def serve_subscriber(conn, addr):
    try:
        handshake(conn, b"PUB")
    except (OSError, EOFError, ValueError) as exc:
        log(f"subscriber {addr} handshake failed: {exc}")
        conn.close()
        return
    sub = Subscriber(conn, addr)
    with subs_lock:
        subs.add(sub)
    log(f"subscriber {addr} connected")
    threading.Thread(target=sub.writer, daemon=True).start()
    sub.reader()


def serve_publisher(conn, addr):
    try:
        handshake(conn, b"SUB")
        conn.sendall(encode_frame(b"\x01"))  # subscribe to everything
        log(f"publisher {addr} connected")
        while True:
            frames, command = read_message(conn)
            if command is not None:
                answer_command(conn, command)
                continue
            stats["in"] += 1
            data = b"".join(encode_frame(f, more=i < len(frames) - 1) for i, f in enumerate(frames))
            with subs_lock:
                targets = [s for s in subs if s.wants(frames[0])]
            for sub in targets:
                sub.offer(data)
    except (OSError, EOFError, ValueError) as exc:
        log(f"publisher {addr} disconnected: {exc}")
    conn.close()


def listen(host, port, handler):
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((host, port))
    srv.listen(64)
    log(f"listening on {host}:{port}")
    while True:
        conn, addr = srv.accept()
        conn.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        threading.Thread(target=handler, args=(conn, f"{addr[0]}:{addr[1]}"), daemon=True).start()


def report():
    while True:
        time.sleep(60)
        log(f"events in={stats['in']} out={stats['out']} dropped={stats['dropped']} subscribers={len(subs)}")


threading.Thread(target=listen, args=("127.0.0.1", UPSTREAM_PORT, serve_publisher), daemon=True).start()
threading.Thread(target=report, daemon=True).start()
listen("0.0.0.0", PORT, serve_subscriber)
