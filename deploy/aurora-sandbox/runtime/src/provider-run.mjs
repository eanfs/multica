// Create-once provider-run client and orchestration.
//
// The server, not the sandbox, owns whether a billable create may run. A retry
// reuses the recorded run; an ambiguous run fails closed instead of creating a
// second time.

import { PROVIDER_RUN_OPERATIONS, sanitizeError } from './policy.mjs';

export class ProviderRunError extends Error {}

function basePath(taskId) {
  return '/api/agent/tasks/' + encodeURIComponent(taskId) + '/aurora-provider-runs';
}

export function createProviderRunClient({ serverOrigin, taskToken, taskId, fetchImpl = globalThis.fetch, timeoutMs = 30000 }) {
  async function request(method, path, body) {
    const response = await fetchImpl(serverOrigin + path, {
      method,
      headers: {
        authorization: 'Bearer ' + taskToken,
        'content-type': 'application/json',
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: Number.isInteger(timeoutMs) && timeoutMs > 0 ? AbortSignal.timeout(timeoutMs) : undefined,
    });
    const text = await response.text();
    if (response.status === 404) {
      const error = new ProviderRunError('provider run not found');
      error.status = 404;
      throw error;
    }
    if (response.status >= 400) {
      throw sanitizeError(new ProviderRunError('provider run request failed with status ' + response.status + ': ' + text.slice(0, 512)));
    }
    try {
      return text ? JSON.parse(text) : {};
    } catch {
      throw new ProviderRunError('provider run response was not valid JSON');
    }
  }

  return {
    begin({ provider, operation, model, args }) {
      return request('POST', basePath(taskId) + '/begin', {
        provider,
        operation,
        model,
        arguments: args ?? {},
      });
    },
    recordExternal(operation, externalId) {
      return request('PUT', basePath(taskId) + '/' + encodeURIComponent(operation) + '/external', { external_id: externalId });
    },
    finish(operation, state, errorCode) {
      const body = { state };
      if (errorCode) body.error_code = errorCode;
      return request('PUT', basePath(taskId) + '/' + encodeURIComponent(operation) + '/finish', body);
    },
    get(operation) {
      return request('GET', basePath(taskId) + '/' + encodeURIComponent(operation));
    },
  };
}

// Opens the create lease for a synchronous provider call and refuses a second
// create when the server did not grant one.
export async function beginProviderRun(client, { provider, operation, model, args }) {
  const begun = await client.begin({ provider, operation, model, args });
  if (!begun.create_allowed) {
    if (begun.external_id) return { run: begun, resumed: true, externalId: begun.external_id };
    throw new ProviderRunError('provider run create lease was not granted; refusing a second create');
  }
  return { run: begun, resumed: false, externalId: begun.external_id ?? null };
}

export async function finishProviderRun(client, operation, state, errorCode) {
  try {
    return await client.finish(operation, state, errorCode);
  } catch {
    return null;
  }
}

export { PROVIDER_RUN_OPERATIONS };
