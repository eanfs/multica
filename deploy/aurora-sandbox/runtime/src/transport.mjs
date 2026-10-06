// Bounded provider transport and process execution.

import http from 'node:http';
import { spawn } from 'node:child_process';
import { redactString, sanitizeError } from './policy.mjs';

export function createProviderFetch({ fetchImpl = globalThis.fetch, allowedOrigins = [], timeoutMs, onRequest } = {}) {
  const origins = new Set(allowedOrigins);
  return async function providerFetch(url, init = {}) {
    let parsed;
    try {
      parsed = new URL(url);
    } catch {
      throw new Error('provider request URL is invalid');
    }
    if (origins.size > 0 && !origins.has(parsed.origin)) {
      throw new Error('provider request origin is not on the compiled allowlist');
    }
    if (typeof onRequest === 'function') onRequest(parsed);
    const options = { ...init, redirect: 'manual' };
    if (!options.signal && Number.isInteger(timeoutMs) && timeoutMs > 0) {
      options.signal = AbortSignal.timeout(timeoutMs);
    }
    const response = await fetchImpl(parsed.href, options);
    if (response.status >= 300 && response.status < 400) {
      throw new Error('provider redirect is not allowed');
    }
    return response;
  };
}

// createHttpProxyFetch forwards plain HTTP through the egress sidecar in
// absolute-form. Node's global fetch ignores HTTP_PROXY unless NODE_USE_ENV_PROXY
// is set, and that agent tunnels every scheme through CONNECT -- but the sidecar
// only authorizes the configured server origin as an absolute-form forward
// request (CONNECT to it is refused because the origin scheme is http and the
// CONNECT path is https/443 only). The broker's server-origin clients
// (provider-run, artifact importer) use this fetch; provider HTTPS traffic keeps
// using the global fetch, which CONNECT-tunnels correctly.
//
// A target on NO_PROXY, or any non-HTTP target, falls through to fetchImpl.
export function createHttpProxyFetch({ proxyUrl, noProxy = '', fetchImpl = globalThis.fetch } = {}) {
  if (typeof proxyUrl !== 'string' || proxyUrl.trim() === '') return fetchImpl;
  const proxy = new URL(proxyUrl);
  const bypass = String(noProxy)
    .split(',')
    .map((entry) => entry.trim().toLowerCase())
    .filter((entry) => entry !== '');
  const isBypassed = (hostname) =>
    bypass.some((entry) => entry === '*' || hostname === entry || hostname.endsWith('.' + entry));
  return async function httpProxyFetch(url, init = {}) {
    const raw = typeof url === 'string' ? url : url && url.url;
    let target;
    try {
      target = new URL(raw);
    } catch {
      return fetchImpl(url, init);
    }
    if (target.protocol !== 'http:' || isBypassed(target.hostname)) return fetchImpl(url, init);
    const headers = new Headers(init.headers || {});
    // Host is derived from the target, never from the caller, so a caller
    // cannot retarget the forward request.
    headers.set('host', target.host);
    const body = init.body === undefined || init.body === null ? undefined : init.body;
    const method = init.method || 'GET';
    return await new Promise((resolve, reject) => {
      const request = http.request(
        {
          host: proxy.hostname,
          port: proxy.port || 80,
          method,
          path: target.href,
          headers: Object.fromEntries(headers.entries()),
          signal: init.signal,
        },
        (response) => {
          const chunks = [];
          response.on('data', (chunk) => chunks.push(chunk));
          response.on('end', () => {
            resolve(new Response(Buffer.concat(chunks), { status: response.statusCode, headers: response.headers }));
          });
        },
      );
      request.on('error', (error) => reject(error));
      if (body !== undefined) request.write(body);
      request.end();
    });
  };
}

export async function readBoundedText(response, maxBytes) {
  const declared = response && response.headers && response.headers.get ? Number(response.headers.get('content-length')) : NaN;
  if (Number.isFinite(declared) && declared > maxBytes) throw new Error('provider response exceeds the size cap');
  const text = await response.text();
  if (Buffer.byteLength(text, 'utf8') > maxBytes) throw new Error('provider response exceeds the size cap');
  return text;
}

export async function readBoundedJson(response, maxBytes) {
  const text = await readBoundedText(response, maxBytes);
  try {
    return JSON.parse(text);
  } catch {
    throw new Error('provider response was not valid JSON');
  }
}

const DEFAULT_MAX_OUTPUT_BYTES = 8 * 1024 * 1024;

function minimalEnvironment(env) {
  if (env) return env;
  const path = process.env.PATH || '/usr/local/bin:/usr/bin:/bin';
  return { PATH: path, LANG: 'C.UTF-8' };
}

export function createProcessRunner({ spawnImpl = spawn } = {}) {
  return {
    run({ command, args = [], env, cwd, timeoutMs = 120000, maxBytes = DEFAULT_MAX_OUTPUT_BYTES }) {
      if (typeof command !== 'string' || command.length === 0) {
        return Promise.reject(new Error('process command is required'));
      }
      if (!Array.isArray(args)) return Promise.reject(new Error('process arguments must be an array'));
      return new Promise((resolve, reject) => {
        let child;
        try {
          child = spawnImpl(command, args, {
            cwd,
            env: minimalEnvironment(env),
            shell: false,
            detached: process.platform !== 'win32',
            stdio: ['ignore', 'pipe', 'pipe'],
          });
        } catch (error) {
          reject(sanitizeError(error));
          return;
        }
        const stdout = [];
        const stderr = [];
        let total = 0;
        let settled = false;
        let timedOut = false;

        function terminate() {
          if (child.pid && process.platform !== 'win32') {
            try {
              process.kill(-child.pid, 'SIGKILL');
            } catch {
              /* already gone */
            }
          }
          try {
            child.kill('SIGKILL');
          } catch {
            /* already gone */
          }
        }

        const timer = setTimeout(() => {
          timedOut = true;
          terminate();
        }, timeoutMs);
        if (typeof timer.unref === 'function') timer.unref();

        child.stdout.on('data', (chunk) => {
          total += chunk.length;
          if (total > maxBytes) {
            terminate();
            return;
          }
          stdout.push(chunk);
        });
        child.stderr.on('data', (chunk) => {
          if (stderr.reduce((sum, item) => sum + item.length, 0) > maxBytes) return;
          stderr.push(chunk);
        });
        child.on('error', (error) => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          reject(new Error(redactString('process execution failed: ' + error.message)));
        });
        child.on('close', (code, signal) => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          if (timedOut) {
            // Surface the captured diagnostics: a stalled render otherwise
            // hides the child's stderr behind a bare timeout message.
            const tail = stderr.length ? ': ' + Buffer.concat(stderr).toString('utf8').slice(-1024) : '';
            reject(new Error(redactString('process timed out and its process tree was terminated' + tail)));
            return;
          }
          if (total > maxBytes) {
            reject(new Error('process output exceeded the size cap'));
            return;
          }
          if (code !== 0) {
            reject(new Error(redactString('process exited with code ' + code + (signal ? ' signal ' + signal : '') + (stderr.length ? ': ' + Buffer.concat(stderr).toString('utf8').slice(0, 512) : ''))));
            return;
          }
          resolve({ stdout: Buffer.concat(stdout).toString('utf8'), stderr: Buffer.concat(stderr).toString('utf8'), code });
        });
      });
    },
  };
}
