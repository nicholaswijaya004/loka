// scripts/k6/hpa.js
//
// Load for the HPA demo: GET /units/{id} ramping up to RATE/s, held for
// HOLD, then stopped. Runs inside the cluster (k8s/load-job.yaml).
//
// noConnectionReuse: kube-proxy balances connections, not requests. With
// keep-alive, every connection opened before a new pod existed stays on the
// old pods, and the new ones get nothing.

import http from 'k6/http';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const RATE = parseInt(__ENV.RATE || '400', 10);
const HOLD = __ENV.HOLD || '4m';
const UNIT = '22222222-2222-2222-2222-222222222222';

export const options = {
  noConnectionReuse: true,
  scenarios: {
    units: {
      executor: 'ramping-arrival-rate',
      startRate: 10,
      timeUnit: '1s',
      preAllocatedVUs: 50,
      maxVUs: 400,
      stages: [
        { target: RATE, duration: '30s' },
        { target: RATE, duration: HOLD },
      ],
    },
  },
  summaryTrendStats: ['med', 'p(95)', 'p(99)', 'max', 'count'],
};

export default function () {
  const res = http.get(`${BASE_URL}/units/${UNIT}`, { tags: { name: 'GET /units/{id}' } });
  check(res, { 'status 200': (r) => r.status === 200 });
}
