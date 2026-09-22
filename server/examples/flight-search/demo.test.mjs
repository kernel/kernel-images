import assert from 'node:assert/strict';
import test from 'node:test';
import {candidatesFor, validatePlan} from './demo.mjs';

test('resolves airports and explicit dates without inventing values', () => {
  const request = 'Find flights from San Francisco to LAX on 2026-10-15';
  assert.deepEqual(candidatesFor(request), {airports: ['SFO', 'LAX'], dates: ['2026-10-15'], ambiguousCities: []});
  assert.equal(validatePlan({origin: 'SFO', destination: 'LAX', departure_date: '2026-10-15'}, candidatesFor(request)), true);
  assert.equal(validatePlan({origin: 'SFO', destination: 'ORD', departure_date: '2026-10-15'}, candidatesFor(request)), false);
});

test('missing and ambiguous airport candidates fail closed', () => {
  assert.deepEqual(candidatesFor('Find flights to LAX on 2026-10-15'), {airports: ['LAX'], dates: ['2026-10-15'], ambiguousCities: []});
  assert.deepEqual(candidatesFor('New York to Los Angeles on 2026-10-15').ambiguousCities, ['New York']);
  assert.deepEqual(candidatesFor('SFO to LAX next week').dates, []);
  assert.deepEqual(candidatesFor('SFO to LAX on 2026-02-30').dates, []);
});
