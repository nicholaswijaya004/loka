import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

export const options = {
  scenarios: {
    contention: {
      executor: 'per-vu-iterations',
      vus: 500,
      iterations: 1,
      maxDuration: '30s',
    },
  },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';

const created = new Counter('bookings_created');
const soldOut = new Counter('bookings_sold_out');
const errors  = new Counter('bookings_error');  

export default function () {
    const res = http.post(`${BASE_URL}/bookings`, JSON.stringify({
        unit_id: '22222222-2222-2222-2222-222222222222',
        customer_id: '11111111-1111-1111-1111-111111111111',   
        qty: 1,
        visit_date_time: '2024-07-01T10:00:00Z',
        }), {
            headers: { 'Content-Type': 'application/json' },
        }
    );

    if (res.status === 201) {
        created.add(1);
    } else if (res.status === 409) {
        soldOut.add(1);
    } else {
        errors.add(1);
        console.log(res.status, res.body);
    }

    check(res, {
        'not a server error': (r) => r.status < 500,
    });
}