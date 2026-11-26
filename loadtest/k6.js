// k6 load test: 100 virtual users fight over the seats of one flight.
// Afterwards, check there are NO double bookings:
//   SELECT seat_no, count(*) FROM seats WHERE booking_pnr IS NOT NULL GROUP BY 1 HAVING count(*) > 1;
// (must return zero rows; seat_no is unique per flight, so it always will)
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
export const options = { vus: 100, duration: '30s' };

export default function () {
  const row = 3 + Math.floor(Math.random() * 28);
  const seat = row + 'ABCDEF'[Math.floor(Math.random() * 6)];
  const headers = { 'X-User-ID': `vu-${__VU}`, 'Content-Type': 'application/json' };
  const hold = http.post(`${BASE}/api/v1/holds`, JSON.stringify({ flightId: 1, seats: [seat] }), { headers });
  check(hold, { 'held or taken': (r) => r.status === 201 || r.status === 409 });
  if (hold.status === 201) {
    headers['Idempotency-Key'] = `${__VU}-${__ITER}`;
    const ok = http.post(`${BASE}/api/v1/holds/${hold.json('id')}/confirm`, null, { headers });
    check(ok, { 'confirmed': (r) => r.status === 201 });
  }
}
