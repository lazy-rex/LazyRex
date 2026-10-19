// Minimal dependency-free Rex client over a Unix socket.
// Control: WebSocket /api/control, subprotocol rex.control.v1 (JSON request/response/event envelopes).
// Data:    WebSocket /api/data, subprotocol rex.data.terminal.v1 (binary HELLO, then snapshot + PTY bytes).
import net from "node:net";
import crypto from "node:crypto";
import { EventEmitter } from "node:events";

export function socketPathFromEnv() {
  const server = process.env.REX_SERVER;
  if (!server?.startsWith("unix://")) throw new Error("set REX_SERVER=unix:///path/to/rex.sock");
  return decodeURIComponent(server.slice("unix://".length));
}

// Opens a WebSocket over a Unix socket. Emits "message" (Buffer, isText) and "close".
export function openWebSocket(socketPath, path, subprotocol) {
  return new Promise((resolve, reject) => {
    const sock = net.connect(socketPath);
    const ws = new EventEmitter();
    const key = crypto.randomBytes(16).toString("base64");
    let buf = Buffer.alloc(0);
    let upgraded = false;

    ws.send = (data, isText = typeof data === "string") => {
      const payload = Buffer.isBuffer(data) ? data : Buffer.from(data);
      const len = payload.length;
      const head = len < 126 ? Buffer.from([0, 0x80 | len])
        : len < 65536 ? Buffer.from([0, 0x80 | 126, len >> 8, len & 255])
        : Buffer.concat([Buffer.from([0, 0x80 | 127]), (() => { const b = Buffer.alloc(8); b.writeBigUInt64BE(BigInt(len)); return b; })()]);
      head[0] = 0x80 | (isText ? 1 : 2);
      const mask = crypto.randomBytes(4);
      const masked = Buffer.from(payload);
      for (let i = 0; i < masked.length; i++) masked[i] ^= mask[i % 4];
      sock.write(Buffer.concat([head, mask, masked]));
    };
    ws.close = () => sock.end();

    sock.on("connect", () => {
      sock.write(`GET ${path} HTTP/1.1\r\nHost: rex\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n` +
        `Sec-WebSocket-Key: ${key}\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: ${subprotocol}\r\n\r\n`);
    });
    sock.on("error", reject);
    sock.on("close", () => ws.emit("close"));
    sock.on("data", (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      if (!upgraded) {
        const end = buf.indexOf("\r\n\r\n");
        if (end < 0) return;
        const head = buf.subarray(0, end).toString();
        if (!head.startsWith("HTTP/1.1 101")) return reject(new Error(`upgrade failed: ${head.split("\r\n")[0]}`));
        upgraded = true;
        buf = buf.subarray(end + 4);
        resolve(ws);
      }
      for (;;) {
        if (buf.length < 2) return;
        const fin = (buf[0] & 0x80) !== 0;
        const op = buf[0] & 0x0f;
        let len = buf[1] & 0x7f, off = 2;
        if (len === 126) { if (buf.length < 4) return; len = buf.readUInt16BE(2); off = 4; }
        else if (len === 127) { if (buf.length < 10) return; len = Number(buf.readBigUInt64BE(2)); off = 10; }
        if (buf.length < off + len) return;
        const payload = Buffer.from(buf.subarray(off, off + len));
        buf = buf.subarray(off + len);
        if (op === 9) { // ping -> pong
          const pong = Buffer.concat([Buffer.from([0x8a, 0x80 | payload.length]), Buffer.alloc(4), payload]);
          sock.write(pong);
        } else if (op === 8) sock.end();
        else if (op === 0 || !fin) {
          // Fragmented messages are not supported. Fail loudly instead of dropping bytes.
          // EventEmitter throws an "error" event that has no listener.
          sock.destroy();
          ws.emit("error", new Error("fragmented WebSocket message: not supported"));
          return;
        } else if (op === 1 || op === 2) ws.emit("message", payload, op === 1);
      }
    });
  });
}

// hello adds fields to the hello frame, for example {client: {kind: "app", name, label}} for an
// app-like client in a lab (lab/palette-probe.mjs).
export async function connectControl(socketPath = socketPathFromEnv(), hello = {}) {
  const ws = await openWebSocket(socketPath, "/api/control", "rex.control.v1");
  const pending = new Map();
  const events = new EventEmitter();
  let nextId = 0;
  const welcome = new Promise((resolve, reject) => {
    // Fail the handshake and every pending call if the connection closes.
    ws.on("close", () => {
      const error = new Error("rex control connection closed");
      reject(error);
      for (const settle of pending.values()) settle({ error: { message: error.message } });
      pending.clear();
    });
    ws.on("message", (data) => {
      const frame = JSON.parse(data.toString());
      if (frame.type === "welcome") resolve(frame);
      else if (frame.type === "response") { pending.get(frame.id)?.(frame); pending.delete(frame.id); }
      else if (frame.type === "event") events.emit("event", frame);
    });
  });
  ws.send(JSON.stringify({ ...hello, type: "hello" }));
  await welcome;
  const call = (method, payload = {}) => new Promise((resolve, reject) => {
    const id = `r${++nextId}`;
    pending.set(id, (frame) => frame.error ? reject(Object.assign(new Error(`${method}: ${frame.error.message}`), { rex: frame.error })) : resolve(frame.payload));
    ws.send(JSON.stringify({ type: "request", id, method, payload }));
  });
  return { call, events, close: () => ws.close() };
}

// Opens a terminal data stream. The ticket must come from data.connect on the SAME principal.
// Rex returns the ticket as base64url text, although the schema describes a byte array.
export async function connectData(socketPath, ticket) {
  const ticketBytes = typeof ticket === "string" ? Buffer.from(ticket, "base64url") : Buffer.from(ticket);
  if (ticketBytes.length !== 16) throw new Error(`expected 16 ticket bytes, got ${ticketBytes.length}`);
  const ws = await openWebSocket(socketPath, "/api/data", "rex.data.terminal.v1");
  ws.send(Buffer.concat([Buffer.from([0x01]), ticketBytes]), false); // HELLO
  return ws;
}
