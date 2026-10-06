// scripts/k6/load.js
//
// Steady, spread load: POST /bookings at RATE/s round-robin over UNITS units,
// plus GET /bookings/{id} at GET_RATE/s and GET /units/{id} at UNIT_RATE/s
// (the availability page, a random unit each time). Open model
// (constant-arrival-rate): k6 starts requests on schedule whether or not
// earlier ones have returned, so a slow server can't quietly lower the load
// and hide its own tail.
//
// Needs scripts/seed-load.sql. Normally driven by scripts/loadrun.sh.
//
//   k6 run -e DURATION=2m -e SUMMARY_FILE=out.json scripts/k6/load.js

import http from 'k6/http';
import { check } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const RATE = parseInt(__ENV.RATE || '50', 10);
const GET_RATE = parseInt(__ENV.GET_RATE || '25', 10);
const DURATION = __ENV.DURATION || '2m';
const UNITS = parseInt(__ENV.UNITS || '1000', 10); // must match seed-load.sql
const CUSTOMERS = parseInt(__ENV.CUSTOMERS || '100', 10); // must match seed-load.sql
const GET_POOL = parseInt(__ENV.GET_POOL || '200', 10); // bookings created in setup for GETs to read
const UNIT_RATE = parseInt(__ENV.UNIT_RATE || '200', 10); // availability-page reads per second

const POST_NAME = 'POST /bookings';
const GET_NAME = 'GET /bookings/{id}';
const UNIT_NAME = 'GET /units/{id}';
const VISIT = '2026-12-01T10:00:00Z';

export const options = {
  scenarios: {
    bookings: {
      executor: 'constant-arrival-rate',
      exec: 'createBooking',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 200,
      maxVUs: 500,
    },
    reads: {
      executor: 'constant-arrival-rate',
      exec: 'getBooking',
      rate: GET_RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 100,
      maxVUs: 200,
    },
    units: {
      executor: 'constant-arrival-rate',
      exec: 'getUnit',
      rate: UNIT_RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 200,
      maxVUs: 500,
    },
  },
  summaryTrendStats: ['min', 'med', 'avg', 'p(90)', 'p(95)', 'p(99)', 'max', 'count'],
  // Thresholds on name-tagged sub-metrics make k6 compute POST and GET
  // separately. The setup requests carry a different name, so they're excluded.
  thresholds: {
    [`http_req_duration{name:${POST_NAME}}`]: ['max>=0'],
    [`http_req_duration{name:${GET_NAME}}`]: ['max>=0'],
    [`http_req_duration{name:${UNIT_NAME}}`]: ['max>=0'],
    [`http_req_failed{name:${POST_NAME}}`]: ['rate==0'],
    [`http_req_failed{name:${GET_NAME}}`]: ['rate==0'],
    [`http_req_failed{name:${UNIT_NAME}}`]: ['rate==0'],
    dropped_iterations: ['count==0'],
  },
};

function pad12(n) {
  return String(n).padStart(12, '0');
}

function unitId(i) {
  return `a0000000-0000-0000-0000-${pad12((i % UNITS) + 1)}`;
}

function customerId(i) {
  return `c0000000-0000-0000-0000-${pad12((i % CUSTOMERS) + 1)}`;
}

function postParams(key, name) {
  return {
    headers: { 'Content-Type': 'application/json', 'Idempotency-Key': key },
    tags: { name },
  };
}

function body(i) {
  return JSON.stringify({ unit_id: unitId(i), customer_id: customerId(i), qty: 1, visit_date_time: VISIT });
}

// setup creates the bookings the GET scenario reads, so GETs hit real rows.
// runId keeps Idempotency-Keys unique across invocations against one database
// (the warm-up and the measured run share a reset).
export function setup() {
  const runId = Date.now().toString(36);
  const ids = [];
  for (let start = 0; start < GET_POOL; start += 20) {
    const reqs = [];
    for (let i = start; i < Math.min(start + 20, GET_POOL); i++) {
      reqs.push(['POST', `${BASE_URL}/bookings`, body(i), postParams(`setup-${runId}-${i}`, 'setup')]);
    }
    for (const res of http.batch(reqs)) {
      if (res.status !== 201) {
        throw new Error(`setup booking failed: ${res.status} ${res.body}`);
      }
      ids.push(res.json('booking_id'));
    }
  }
  return { runId, ids };
}

export function createBooking(data) {
  const i = exec.scenario.iterationInTest;
  const res = http.post(`${BASE_URL}/bookings`, body(i), postParams(`load-${data.runId}-${i}`, POST_NAME));
  check(res, { 'POST 201': (r) => r.status === 201 });
}

export function getBooking(data) {
  const id = data.ids[exec.scenario.iterationInTest % data.ids.length];
  const res = http.get(`${BASE_URL}/bookings/${id}`, { tags: { name: GET_NAME } });
  check(res, { 'GET 200': (r) => r.status === 200 });
}

// A uniform random unit, so each unit's reads arrive independently, as from
// many browsers; round-robin would read every unit at a fixed interval.
export function getUnit() {
  const id = unitId(Math.floor(Math.random() * UNITS));
  const res = http.get(`${BASE_URL}/units/${id}`, { tags: { name: UNIT_NAME } });
  check(res, { 'GET unit 200': (r) => r.status === 200 });
}

function fmt(v) {
  return v === undefined ? '-' : v.toFixed(2);
}

function row(label, m) {
  if (!m) return `${label.padEnd(20)} (no samples)`;
  const v = m.values;
  return [label.padEnd(20), fmt(v.med), fmt(v['p(95)']), fmt(v['p(99)']), fmt(v.max), String(v.count)].map((s, idx) => (idx === 0 ? s : s.padStart(10))).join('');
}

export function handleSummary(data) {
  const m = data.metrics;
  const failed = (name) => {
    const f = m[`http_req_failed{name:${name}}`];
    return f ? `${f.values.passes}/${f.values.passes + f.values.fails}` : '-';
  };
  const dropped = m.dropped_iterations ? m.dropped_iterations.values.count : 0;
  const lines = [
    '',
    `${'latency (ms)'.padEnd(20)}${['p50', 'p95', 'p99', 'max', 'count'].map((s) => s.padStart(10)).join('')}`,
    row(POST_NAME, m[`http_req_duration{name:${POST_NAME}}`]),
    row(GET_NAME, m[`http_req_duration{name:${GET_NAME}}`]),
    row(UNIT_NAME, m[`http_req_duration{name:${UNIT_NAME}}`]),
    '',
    `failed: POST ${failed(POST_NAME)}, GET ${failed(GET_NAME)}, GET unit ${failed(UNIT_NAME)}; dropped iterations: ${dropped}`,
    '',
  ];
  const out = { stdout: lines.join('\n') };
  if (__ENV.SUMMARY_FILE) {
    out[__ENV.SUMMARY_FILE] = JSON.stringify(data, null, 2);
  }
  return out;
}
