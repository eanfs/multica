// Bounded provider transport and process execution.

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
            reject(new Error('process timed out and its process tree was terminated'));
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
