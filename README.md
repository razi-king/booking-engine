# 🎫 Booking Engine

[![CI](https://github.com/razi-king/booking-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/razi-king/booking-engine/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-4169E1?logo=postgresql&logoColor=white)

A flight seat-booking engine in Go and PostgreSQL that **can never sell the same seat twice**, even when hundreds of people click "Book" at the same moment.

**Live seat-map demo:** https://razi-king.github.io/booking-engine/ (runs in demo mode in your browser; add `?api=<url>` to use a deployed API)

## The problem it solves

Two users click the last window seat in the same millisecond. Naive "check if free, then write" code tells **both** they got it. This repo includes a test that proves it: with the naive approach, **10 out of 10** concurrent users were told they won seat 15A. With this engine's row locks, **exactly 1 of 50** wins, every time.

| Problem | Solution here |
|---|---|
| Two users book the same seat | `SELECT … FOR UPDATE` row locks inside a transaction |
| Deadlocks when users lock overlapping seats | seats always locked in the same (row, letter) order |
| User takes 8 minutes to pay | 10-minute **seat holds**, then confirm |
| User closes the tab | holds **expire**: lazily on every read, and eagerly by a min-heap scheduler |
| Mobile network retries "Pay" | **Idempotency-Key** header: a retry replays the first response |
| Booking reference | 6-character **PNR** from an unambiguous 32-letter alphabet (no 0/O, 1/I) |

## Design highlights

- **Two stores, one interface, one test suite.** `store/postgres` (production) and `store/memory` (demo and fast tests) both pass the same contract tests in `store/storetest`, including the 50-user race and the deadlock test.
- **Indexed min-heap scheduler** wakes exactly when the next hold expires (O(log n) add and remove), instead of polling the database every second.
- **SOLID:** small interfaces (`FlightReader`, `HoldStore`, `BookingStore`, `IdempotencyStore`), a composition root in `cmd/api`, injected clocks and id generators.
- **DRY:** one error→HTTP mapping table, one `inTx` helper for every transaction, one pricing rule, one seed catalogue for both stores.

## API

All user endpoints need an `X-User-ID` header (in production this comes from a JWT checked by an API gateway).

| Method | Path | What it does |
|---|---|---|
| GET | `/api/v1/flights` | list flights |
| GET | `/api/v1/flights/{id}/seats` | seat map: rows × letters with status and price |
| POST | `/api/v1/holds` | hold seats `{"flightId":1,"seats":["12A","12B"]}` → 201, or 409 with the taken seats |
| GET | `/api/v1/holds/{id}` | hold status (`active`, `expired`, …) |
| POST | `/api/v1/holds/{id}/confirm` | confirm → booking with PNR (send `Idempotency-Key`) |
| DELETE | `/api/v1/holds/{id}` | release early |
| GET | `/api/v1/bookings/{pnr}` | look up a booking |
| POST | `/api/v1/bookings/{pnr}/cancel` | cancel; seats go back on sale |
| GET | `/healthz`, `/metrics` | health (pings Postgres), scheduler counters |

```bash
curl -X POST localhost:8080/api/v1/holds -H 'X-User-ID: asha' -d '{"flightId":1,"seats":["12A"]}'
curl -X POST localhost:8080/api/v1/holds/<id>/confirm -H 'X-User-ID: asha' -H 'Idempotency-Key: pay-1'
```

## Run it

```bash
make run        # in-memory store, http://localhost:8080 (seat map at /)
make up         # Postgres + API with Docker Compose
make test-pg    # every test, including Postgres contract tests (needs Postgres on :5432)
make bench      # heap vs sorted slice
```

| Env var | Default | |
|---|---|---|
| `DATABASE_URL` | *(empty → memory)* | `postgres://user:pass@host:5432/db?sslmode=disable` |
| `HOLD_TTL` | `10m` | |
| `MAX_SEATS` | `9` | per hold |
| `SWEEP_EVERY` | `1m` | fallback sweep interval |
| `IDEMPOTENCY_RETENTION` | `24h` | |
| `PORT`, `ALLOWED_ORIGINS` | `8080`, `*` | |

## Layout

```
cmd/api/              composition root, graceful shutdown
internal/domain/      flights, seats, holds, bookings, seat rules, errors
internal/catalog/     demo flights and seats (shared by both stores)
internal/store/       interfaces; memory/ and postgres/ implementations; storetest/ contract suite
internal/booking/     service rules, PNR generator, expiry heap + scheduler
internal/httpapi/     handlers, idempotency middleware, error mapping
web/                  seat-map page (served by the API and by GitHub Pages)
```

---
Built by **Razi King** · Go backend engineer in travel tech · see also [flight-search-aggregator](https://github.com/razi-king/flight-search-aggregator)
