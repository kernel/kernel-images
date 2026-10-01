// Ported from pi's codemode `models` tests
// (packages/coding-agent/test/suite/agent-session-codemode.test.ts at
// https://github.com/earendil-works/pi/tree/v0.99.2, MIT License,
// Copyright (c) 2025 Mario Zechner).
import assert from 'node:assert/strict';
import test from 'node:test';

import {
  type ClassifierResult,
  createModels,
  createProvider,
  envApiKeyAuth,
} from '@earendil-works/pi-ai';

import { createModelsNamespace } from './models.ts';

const scorerModel = {
  type: 'classifier' as const,
  id: 'judge',
  name: 'Judge',
  api: 'test-classifier',
  provider: 'scorer',
  baseUrl: 'https://classifier.test/v1',
  input: ['text' as const],
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  contextWindow: 1000,
  headers: { 'X-Secret': 'hunter2' },
};

const questions = {
  approved: { type: 'bool' as const, instructions: 'Approval?', criteria: { true: 'yes', false: 'no' } },
};

function setup(signal?: AbortSignal) {
  const observed: { baseUrl: string; apiKey: string | undefined; text: unknown }[] = [];
  let active = 0;
  let maxActive = 0;
  const runtime = createModels();
  runtime.setProvider(
    createProvider({
      id: 'scorer',
      auth: { apiKey: envApiKeyAuth('Scorer API key', ['SCORER_API_KEY']) },
      models: [scorerModel],
      classifiers: {
        'test-classifier': {
          classify: async (model, context, options): Promise<ClassifierResult> => {
            active++;
            maxActive = Math.max(maxActive, active);
            await new Promise((resolve) => setTimeout(resolve, 10));
            active--;
            const text = context.state.text;
            observed.push({ baseUrl: model.baseUrl, apiKey: options?.apiKey, text });
            if (text === 'explode') {
              return {
                api: model.api,
                provider: model.provider,
                model: model.id,
                answers: {},
                stopReason: 'error',
                errorMessage: 'classifier exploded',
                timestamp: 0,
              };
            }
            return {
              api: model.api,
              provider: model.provider,
              model: model.id,
              answers: { approved: { type: 'bool', probability: text === 'good' ? 0.9 : 0.1 } },
              stopReason: 'stop',
              timestamp: 0,
            };
          },
        },
      },
    }),
  );
  let loads = 0;
  const models = createModelsNamespace({
    load: async () => {
      loads++;
      return runtime;
    },
    signal: () => signal,
  });
  return { models, observed, maxActive: () => maxActive, loads: () => loads };
}

test('lists models and classifies with environment auth, ignoring script-supplied fields', async (t) => {
  t.after(() => delete process.env.SCORER_API_KEY);
  const { models, observed, maxActive, loads } = setup();

  assert.deepEqual(await models.getAvailableOfType('classifier', 'scorer'), []);
  process.env.SCORER_API_KEY = 'secret-key';

  const [model] = await models.getAvailableOfType('classifier', 'scorer');
  const listed = await models.getModelsOfType('classifier');
  const same = await models.getModelOfType('classifier', 'scorer', 'judge');
  const texts = ['good', 'bad', 'good', 'bad', 'good', 'bad'];
  const results = await Promise.all(
    texts.map((text) =>
      models.classify({ ...model, baseUrl: 'https://evil.test' }, { state: { text }, questions }),
    ),
  );

  assert.equal(model.id, 'judge');
  assert.equal('headers' in model, false);
  assert.ok(listed.some((entry) => entry.provider === 'scorer' && entry.id === 'judge'));
  assert.equal(same?.id, 'judge');
  assert.equal(await models.getModelOfType('classifier', 'scorer', 'nope'), undefined);
  assert.deepEqual(
    results.map((result) => (result.answers.approved as { probability: number }).probability),
    [0.9, 0.1, 0.9, 0.1, 0.9, 0.1],
  );
  assert.equal(observed.length, 6);
  assert.ok(observed.every((entry) => entry.baseUrl === 'https://classifier.test/v1' && entry.apiKey === 'secret-key'));
  // Six classifications with at most four in flight.
  assert.equal(maxActive(), 4);
  assert.equal(loads(), 1);
});

test('reports provider errors as results and invalid arguments as exceptions', async (t) => {
  process.env.SCORER_API_KEY = 'secret-key';
  t.after(() => delete process.env.SCORER_API_KEY);
  const { models } = setup();

  const model = await models.getModelOfType('classifier', 'scorer', 'judge');
  const failed = await models.classify(model, { state: { text: 'explode' }, questions });
  assert.deepEqual([failed.stopReason, failed.errorMessage], ['error', 'classifier exploded']);
  await assert.rejects(models.getModelsOfType('video'), /Unknown model type "video"/);
  await assert.rejects(models.classify({ provider: 'scorer', id: 'nope' }, {}), /^Error: Unknown classifier model "scorer\/nope"$/);
  await assert.rejects(models.classify('judge', {}), /expects a model/);
  await assert.rejects(models.getModelOfType('classifier', 'scorer'), /expects a type, a provider, and an id/);
});

test('passes the active execution signal to the provider', async () => {
  const controller = new AbortController();
  controller.abort(new Error('execution finished'));
  const { models } = setup(controller.signal);
  const model = await models.getModelOfType('classifier', 'scorer', 'judge');
  const result = await models.classify(model, { state: { text: 'good' }, questions });
  assert.equal(result.stopReason, 'aborted');
});

test('retries loading after a failed load', async () => {
  let attempts = 0;
  const models = createModelsNamespace({
    load: async () => {
      attempts++;
      if (attempts === 1) throw new Error('load failed');
      return createModels();
    },
    signal: () => undefined,
  });
  await assert.rejects(models.getModelsOfType('chat'), /load failed/);
  assert.deepEqual(await models.getModelsOfType('chat'), []);
  assert.equal(attempts, 2);
});
