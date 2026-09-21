import assert from 'node:assert/strict';
import test from 'node:test';
import { matchesURLPattern } from './custom-webmcp.ts';

test('matches URL patterns across top-level and nested frame URLs', () => {
  assert.equal(
    matchesURLPattern(
      'https://js.stripe.com/v3/checkout-inner-origin-frame-abc.html#state',
      'https://js.stripe.com/v3/checkout-inner-origin-frame-*',
    ),
    true,
  );
  assert.equal(
    matchesURLPattern(
      'https://assets.braintreegateway.com/web/3.120.0/html/hosted-fields-frame.html',
      'https://assets.braintreegateway.com/web/*',
    ),
    true,
  );
  assert.equal(
    matchesURLPattern(
      'https://example.com/v3/checkout-inner-origin-frame-abc.html',
      'https://js.stripe.com/v3/checkout-inner-origin-frame-*',
    ),
    false,
  );
});

test('supports wildcard schemes and hosts', () => {
  assert.equal(matchesURLPattern('https://pay.example.com/form', '*://*.example.com/*'), true);
  assert.equal(matchesURLPattern('http://pay.example.com/form', '*://*.example.com/*'), true);
  assert.equal(matchesURLPattern('https://example.org/form', '*://*.example.com/*'), false);
});
