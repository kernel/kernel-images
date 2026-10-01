// The `models` namespace: the model catalog and classifier models, backed by
// @earendil-works/pi-ai. Credentials resolve from the REPL's environment at
// call time; set them with PUT /repl/env.
//
// Adapted from pi's codemode `models` globals (createModelGlobals and its
// helpers in packages/coding-agent/src/extensions/codemode/execute.ts, at
// https://github.com/earendil-works/pi/tree/v0.99.2). pi's nested-call rows and
// session cost accounting are not carried over.
//
// Copyright (c) 2025 Mario Zechner
// Licensed under the MIT License:
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import type { AnyModel, ClassifierContext, ClassifierResult, ModelType, Models } from '@earendil-works/pi-ai' with {
  'resolution-mode': 'import',
};

/** The part of the model collection that scripts reach through `models`. */
export type ModelsRuntime = Pick<Models, 'getModelsOfType' | 'getAvailableOfType' | 'getModelOfType' | 'classify'>;

/** Classifier calls one REPL may have in flight; `Promise.all` over many items queues the rest. */
const MAX_CONCURRENT_MODEL_CALLS = 4;
const MODEL_TYPES: ReadonlySet<string> = new Set<ModelType>(['chat', 'image', 'classifier']);

function toModelType(value: unknown): ModelType {
  if (typeof value === 'string' && MODEL_TYPES.has(value)) return value as ModelType;
  throw new Error(`Unknown model type ${JSON.stringify(value)}. Use "chat", "image", or "classifier".`);
}

function toProvider(value: unknown): string | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== 'string') throw new Error('provider must be a string');
  return value;
}

/** Catalog entry for scripts. `headers` is dropped because provider headers can carry credentials. */
function toModelInfo(model: AnyModel): Record<string, unknown> {
  const info: Record<string, unknown> = { ...model };
  delete info.headers;
  return info;
}

/** Runs at most `limit` calls at once, in call order. */
function createLimiter(limit: number): <T>(run: () => Promise<T>) => Promise<T> {
  let active = 0;
  const waiting: (() => void)[] = [];
  return async (run) => {
    if (active >= limit) await new Promise<void>((resolve) => waiting.push(resolve));
    active++;
    try {
      return await run();
    } finally {
      active--;
      waiting.shift()?.();
    }
  };
}

export interface ModelsNamespaceOptions {
  /** Loads the model collection on first use. */
  load: () => Promise<ModelsRuntime>;
  /** Abort signal of the active execution, if any. */
  signal: () => AbortSignal | undefined;
}

/** Loads pi-ai's built-in providers only when a script first calls `models.*`. */
export function loadBuiltinModels(): Promise<ModelsRuntime> {
  return import('@earendil-works/pi-ai/providers/all').then(({ builtinModels }) => builtinModels());
}

export function createModelsNamespace(options: ModelsNamespaceOptions) {
  let runtime: Promise<ModelsRuntime> | undefined;
  const models = () => {
    runtime ??= options.load().catch((error) => {
      runtime = undefined;
      throw error;
    });
    return runtime;
  };
  const limit = createLimiter(MAX_CONCURRENT_MODEL_CALLS);
  return Object.freeze({
    async getModelsOfType(type: unknown, provider?: unknown) {
      return (await models()).getModelsOfType(toModelType(type), toProvider(provider)).map(toModelInfo);
    },
    async getAvailableOfType(type: unknown, provider?: unknown) {
      const available = await (await models()).getAvailableOfType(toModelType(type), toProvider(provider), {
        signal: options.signal(),
      });
      return available.map(toModelInfo);
    },
    async getModelOfType(type: unknown, provider: unknown, id: unknown) {
      if (typeof provider !== 'string' || typeof id !== 'string') {
        throw new Error('models.getModelOfType() expects a type, a provider, and an id');
      }
      const model = (await models()).getModelOfType(toModelType(type), provider, id);
      return model === undefined ? undefined : toModelInfo(model);
    },
    async classify(model: unknown, context: unknown): Promise<ClassifierResult> {
      const ref = model as { provider?: unknown; id?: unknown } | null;
      if (typeof ref !== 'object' || ref === null || typeof ref.provider !== 'string' || typeof ref.id !== 'string') {
        throw new Error('models.classify() expects a model from models.getModelOfType() or models.getAvailableOfType()');
      }
      // Only provider and id count. A script-supplied baseUrl or headers must never receive the credentials.
      const runtime = await models();
      const resolved = runtime.getModelOfType('classifier', ref.provider, ref.id);
      if (!resolved) throw new Error(`Unknown classifier model "${ref.provider}/${ref.id}"`);
      const signal = options.signal();
      return limit(() => runtime.classify(resolved, context as ClassifierContext, { signal }));
    },
  });
}
