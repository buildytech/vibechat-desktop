'use strict';
// BuildY VibeChat: loaded into Pi with `node --require`. The jail has a network namespace of its own with nothing in
// it; the only way out is a Unix socket the host binds in, which leads to the provider gateway. Pi's HTTP client
// speaks to 127.0.0.1:<port> inside the namespace, and this relay carries the bytes to the socket. No secret passes
// through here or exists in the jail: the socket itself is the credential, and the agent's shell (a nested jail with
// no network and without the socket) cannot reach it.
const net = require('net');
const sock = process.env.VIBECHAT_GW_SOCK;
const port = Number(process.env.VIBECHAT_GW_PORT);
if (sock && port) {
  const srv = net.createServer((c) => {
    const u = net.connect(sock);
    const end = () => { c.destroy(); u.destroy(); };
    c.on('error', end); u.on('error', end); c.on('close', end); u.on('close', end);
    c.pipe(u); u.pipe(c);
  });
  srv.on('error', (e) => { process.stderr.write('vibechat relay: ' + e.message + '\n'); process.exit(70); });
  srv.listen(port, '127.0.0.1');
  srv.unref();
}
