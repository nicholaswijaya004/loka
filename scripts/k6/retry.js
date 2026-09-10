import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

export const options = {
  scenarios: {
    retry: {
      executor: 'per-vu-iterations',
      vus: 500,
      iterations: 1,
      maxDuration: '30s',
    },
  },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';

const created = new Counter('bookings_created');
const replayed = new Counter('bookings_replayed');
const duplicateRejected = new Counter('duplicate_rejected');
const errors  = new Counter('bookings_error');  

export default function () {
    const res = http.post(`${BASE_URL}/bookings`, JSON.stringify({
        unit_id: '22222222-2222-2222-2222-222222222222',
        customer_id: '11111111-1111-1111-1111-111111111111',   
        qty: 1,
        visit_date_time: '2024-07-01T10:00:00Z',
        }), {
            headers: {
                'Content-Type': 'application/json',
                'Idempotency-Key': 'retry-test-fixed-key-001',
            },
        }
    );

    if (res.status === 201) {
        created.add(1);
    } else if (res.status === 200) {
        replayed.add(1);
    } else if (res.status === 409) {
        duplicateRejected.add(1);
    } else {
        errors.add(1);
        console.log(res.status, res.body);
    }

    check(res, {
        'not a server error': (r) => r.status < 500,
    });
}
