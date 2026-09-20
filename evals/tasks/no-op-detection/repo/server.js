const DEFAULT_TIMEOUT_MS = 30000;

function createServer(options = {}) {
  const timeout = options.timeout ?? DEFAULT_TIMEOUT_MS;
  return { timeout, started: false };
}

function start(server) {
  server.started = true;
  return server;
}

module.exports = { createServer, start, DEFAULT_TIMEOUT_MS };
