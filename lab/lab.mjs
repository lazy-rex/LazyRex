import { realpath } from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';

// Resolve symlinks before connecting. Tests only accept sockets beneath a temporary directory.
export async function labSocket(server = process.env.REX_SERVER) {
  if (!server?.startsWith('unix:///')) throw new Error('Set REX_SERVER to an explicit isolated Unix lab socket');
  const socket = await realpath(decodeURIComponent(server.slice(7)));
  const roots = await Promise.all(['/tmp', os.tmpdir()].map(root => realpath(root)));
  if (!roots.some(root => socket.startsWith(root + path.sep)) || !socket.endsWith('/rex.sock')) {
    throw new Error('Refusing a socket outside a temporary lab directory');
  }
  return socket;
}

export async function until(check, description, timeout = 15000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    const result = await check();
    if (result) return result;
    await new Promise(resolve => setTimeout(resolve, 40));
  }
  throw new Error(`Timed out: ${description}`);
}

export async function discoverHub(control) {
  const {results} = await control.call('target.resolve', {kinds:['client'], queries:['lazyrex-hub']});
  const result = results?.[0];
  const candidates = result?.status === 'resolved' ? [result.target]
    : result?.status === 'ambiguous' ? result.candidates : [];
  const matches = [];
  for (const candidate of candidates) {
    const client = await control.call('client.inspect', {client_id:candidate.client_id});
    if (client.client?.connection_state === 'connected') matches.push(candidate);
  }
  if (matches.length !== 1) throw new Error(`Hub discovery failed: ${matches.length} connected candidates`);
  return matches[0].client_id;
}
