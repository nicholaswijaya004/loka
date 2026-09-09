import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
  vus: 5,
  duration: '10s',
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<200'],
  },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';

export default function () {
  const res = http.get(`${BASE_URL}/healthz`);

  check(res, {
    'status is 200': (r) => r.status === 200,
    'body says ok': (r) => r.json('status') === 'ok',
  });

  sleep(1);
}
