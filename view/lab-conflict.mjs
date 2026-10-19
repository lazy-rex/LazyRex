// Run only against an isolated lab. Both new views must have Back focused.
import { connectControl } from "../../lib/rex-client.mjs";

const [socket, session, first, second] = process.argv.slice(2);
if (!socket?.startsWith("/tmp/rexlab-") || !session || !first || !second) {
  throw new Error("Usage: node lab-conflict.mjs /tmp/rexlab-.../rex.sock <session> <first block> <second block>");
}
const rex = await connectControl(socket);
try {
  await rex.call("session.attach", { session_id: session, event_scope: "session" });
  const results = await Promise.all([
    [first, "\t-conflict-A"],
    [second, "\t-conflict-B"],
  ].map(([block, data]) => rex.call("com.superlogical.terminal.write", {
    session_id: session, block_id: block, args: { data },
  })));
  console.log(JSON.stringify({ results }));
} finally {
  rex.close();
}
