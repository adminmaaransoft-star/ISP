// k6_api_load.js — API load test (AAA-005 / NFR-PERF-001)
// Target: p99 API response ≤ 200 ms at 100 VU
// Usage: k6 run scripts/k6_api_load.js
//        k6 run -e BASE_URL=http://localhost:8080 scripts/k6_api_load.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const JWT_TOKEN = __ENV.JWT_TOKEN || '';

const apiLatency = new Trend('api_latency_ms', true);
const errorRate = new Rate('api_errors');

export const options = {
  scenarios: {
    constant_load: {
      executor: 'constant-vus',
      vus: 100,
      duration: '60s',
    },
  },
  thresholds: {
    // NFR-PERF-001: p99 ≤ 200 ms for API
    'api_latency_ms{endpoint:health}': ['p(99)<200'],
    'api_latency_ms{endpoint:subscriber_get}': ['p(99)<200'],
    'api_errors': ['rate<0.01'],   // error rate < 1%
    'http_req_failed': ['rate<0.01'],
  },
};

const headers = {
  'Content-Type': 'application/json',
  ...(JWT_TOKEN ? { 'Authorization': `Bearer ${JWT_TOKEN}` } : {}),
};

export default function () {
  // Health check — no auth needed
  const healthRes = http.get(`${BASE_URL}/health`);
  apiLatency.add(healthRes.timings.duration, { endpoint: 'health' });
  errorRate.add(!check(healthRes, { 'health 200': (r) => r.status === 200 }));

  // Subscriber get (requires valid JWT)
  if (JWT_TOKEN) {
    const subRes = http.get(`${BASE_URL}/api/v1/subscribers/1`, { headers });
    apiLatency.add(subRes.timings.duration, { endpoint: 'subscriber_get' });
    errorRate.add(!check(subRes, {
      'subscriber get 200 or 404': (r) => r.status === 200 || r.status === 404,
    }));
  }

  sleep(0.1);
}
