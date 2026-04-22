# Issues

> Last audited: 2026-04-22

| ID | Severity | Status | Title |
|----|----------|--------|-------|
| IH-001 | high | fixed | Devices page calls `GET /v1/devices` which is never registered on the backend |
| IH-002 | medium | fixed | JSON-RPC success response drops `result` field when the result is nil |
| IH-003 | medium | fixed | `UploadFile` stores files before validating that the segment is a valid integer |
| IH-004 | low | fixed | `Transcoder.Start` releases the mutex during `wg.Wait()`, exposing a concurrent-start race |
| IH-005 | low | fixed | `ListRoutesByDevicePaginated` has no tiebreaker, so paging through routes with equal `created_at` can skip or duplicate rows |
| IH-006 | low | fixed | `HandleWebSocket` returns an error after `upgrader.Upgrade` already wrote the HTTP error response |
| IH-007 | high | open | `TripHandler.GetStats` and `GetTripByRoute` reject every session-authenticated request with 403 |
| IH-008 | high | open | `SignalsHandler.GetRouteSignals` rejects every session-authenticated request with 403 |

---

## IH-001: Devices page calls `GET /v1/devices` which is never registered on the backend

- **Severity**: high
- **Category**: api-misuse
- **Location**: `web/src/app/devices/page.tsx:40`, `cmd/server/main.go:60-89`
- **Status**: fixed
- **Description**: The frontend devices page fetches `/v1/devices` expecting a `Device[]` listing:

  ```ts
  const data = await apiFetch<Device[]>("/v1/devices");
  ```

  No such route is registered in `cmd/server/main.go`. The only registered device routes are `GET /v1.1/devices/:dongle_id` (single device by ID via `DeviceHandler.RegisterRoutes`), `GET /v1/devices/:dongle_id/params` (parameter listing via `ConfigHandler.RegisterRoutes`), and their PUT/DELETE variants. Hitting `/v1/devices` returns 404 from Echo's default handler, so the devices page always renders the "Failed to load devices" error state. `db.ListDevices` exists in the generated sqlc code but is never wired into an HTTP handler.

- **Suggested fix**: Add a `ListDevices` handler in `internal/api/device.go`, register it at `g.GET("/devices", h.ListDevices)` (or a dedicated group), and either relax auth for this route or mount the frontend call under an authenticated path the frontend can use. The handler should return `[]deviceResponse` (or a paginated envelope analogous to `routeListResponse`) built from `queries.ListDevices(ctx)`.

---

## IH-002: JSON-RPC success response drops `result` field when the result is nil

- **Severity**: medium
- **Category**: api-misuse
- **Location**: `internal/ws/rpc.go:28-33`
- **Status**: fixed
- **Description**: `RPCResponse` declares both `Result` and `Error` with `omitempty`:

  ```go
  type RPCResponse struct {
      JSONRPC string          `json:"jsonrpc"`
      Result  interface{}     `json:"result,omitempty"`
      Error   *RPCError       `json:"error,omitempty"`
      ID      json.RawMessage `json:"id"`
  }
  ```

  When a `MethodHandler` returns `(nil, nil)` — a legitimate "success, no payload" response, e.g. a ping or fire-and-forget style method — `NewRPCResponse(id, nil)` produces a payload with neither `result` nor `error` set. `json.Marshal` therefore emits `{"jsonrpc":"2.0","id":1}`, which violates JSON-RPC 2.0 §5 ("either the `result` member or the `error` member MUST be included, but both members MUST NOT be included"). A strict client (including openpilot's athenad) may reject it or treat it as an error.

  None of the current stub handlers in `methods.go` returns nil, but the interface is exported (`MethodHandler`) and this is a trap for any handler author.

- **Suggested fix**: Remove `omitempty` from `Result` and serialize successful responses by other means. One option is a custom `MarshalJSON` on `RPCResponse` that writes `"result":null` (no `error`) for successes and `"error":{...}` (no `result`) for failures. The simplest change — drop `omitempty` from `Result` — works for the success path but will also emit `"result":null` in error responses, which some strict clients reject. A custom marshaller that inspects `resp.Error` is safer.

---

## IH-003: `UploadFile` stores files before validating that the segment is a valid integer

- **Severity**: medium
- **Category**: logic
- **Location**: `internal/api/upload.go:126-180` and `trackUpload` at `upload.go:184-188`
- **Status**: fixed
- **Description**: `parseUploadPath` only splits the URL by `/` and checks that no component is empty — it does not parse the segment component as an integer. `UploadFile` then calls `h.storage.Store(dongleID, route, segment, filename, body)`, writing the upload to disk *before* any database tracking happens. Only later does `trackUpload` call `strconv.Atoi(segmentStr)`:

  ```go
  segmentNum, err := strconv.Atoi(segmentStr)
  if err != nil {
      return fmt.Errorf("invalid segment number %q: %w", segmentStr, err)
  }
  ```

  When the segment component is non-numeric (e.g. `/upload/abc/2024-03-15--12-30-00/foo/rlog`), the file is already written under `STORAGE_PATH/abc/2024-03-15--12-30-00/foo/rlog`, `trackUpload` then logs a warning and returns, and the handler returns `200 OK`. The result is an untracked blob on disk under an unexpected directory name, plus a client that believes its upload succeeded.

- **Suggested fix**: Parse and validate the segment as an integer inside `parseUploadPath` (or add a dedicated check in `UploadFile` before calling `Storage.Store`), returning 400 when it isn't a non-negative integer. This matches the validation already present for `validFilenames`.

---

## IH-004: `Transcoder.Start` releases the mutex during `wg.Wait()`, exposing a concurrent-start race

- **Severity**: low
- **Category**: race-condition
- **Location**: `internal/worker/transcoder.go:67-85`
- **Status**: fixed
- **Description**: `Start` briefly releases `t.mu` so the waiting caller does not hold the lock across `t.wg.Wait()`:

  ```go
  t.mu.Lock()
  if t.cancel != nil {
      t.cancel()
      t.mu.Unlock()
      t.wg.Wait()
      t.mu.Lock()
  }
  ctx, t.cancel = context.WithCancel(ctx)
  t.mu.Unlock()

  for i := 0; i < t.concurrency; i++ {
      t.wg.Add(1)
      ...
  }
  ```

  A concurrent `Start` call can slip in during the unlocked window, observe the old (already-cancelled) `t.cancel`, call it as a no-op, return from its own `wg.Wait()` with a zero counter, assign its new `cancel`, and spawn workers. The original call then resumes, overwrites `t.cancel` with yet another cancel func, and spawns more workers. The workers spawned by the first call become orphaned — `Stop` can only cancel via the currently-stored `t.cancel`, so the first batch only exits when the parent context does. There is also a subtle `sync.WaitGroup` misuse: `wg.Add(1)` is called after the lock is released, concurrently with another goroutine's `wg.Wait()`.

  In practice `Start`/`Stop` are invoked sequentially in `cmd/server/main.go` (not at all right now), so the race is latent. It would bite a caller that orchestrates the transcoder from multiple goroutines.

- **Suggested fix**: Either document that `Start` must not be called concurrently with itself or `Stop`, or guard against it by holding the mutex across the whole lifecycle transition. A clean fix is to introduce a `started bool` guarded by `mu`, reject duplicate starts with a panic or no-op, and move the worker-spawn loop inside the locked region.

---

## IH-005: `ListRoutesByDevicePaginated` has no tiebreaker, so paging through routes with equal `created_at` can skip or duplicate rows

- **Severity**: low
- **Category**: logic
- **Location**: `sql/queries/routes.sql` (`ListRoutesByDevicePaginated` and `ListRoutesByDeviceWithCounts`), also duplicated in `internal/db/routes_custom.go:22-29`
- **Status**: fixed
- **Description**: Both list queries order by `created_at DESC` with `LIMIT`/`OFFSET` but provide no secondary sort key:

  ```sql
  ORDER BY r.created_at DESC
  LIMIT $2 OFFSET $3
  ```

  PostgreSQL does not guarantee a stable order between two executions when sort keys tie, so when multiple routes share `created_at` (e.g. two routes inserted in the same millisecond by the `trackUpload` race that creates routes on demand), paginating through the list can return the tied rows in different positions and either skip a route or return it twice across pages. `trackUpload` creates routes with only `dongle_id` and `route_name` populated, so `created_at` defaults to `now()` — under concurrent uploads this is exactly when ties are most likely.

- **Suggested fix**: Add a deterministic secondary sort key, e.g. `ORDER BY r.created_at DESC, r.id DESC`. Update both the sqlc query in `sql/queries/routes.sql` and the hand-rolled version in `internal/db/routes_custom.go`, then regenerate sqlc.

---

## IH-006: `HandleWebSocket` returns an error after `upgrader.Upgrade` already wrote the HTTP error response

- **Severity**: low
- **Category**: api-misuse
- **Location**: `internal/ws/handler.go:83-86`
- **Status**: fixed
- **Description**: When the upgrade fails, gorilla/websocket's `Upgrade` already calls `http.Error` on the response writer before returning the error. The handler then wraps and returns it:

  ```go
  conn, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
  if err != nil {
      return fmt.Errorf("failed to upgrade connection: %w", err)
  }
  ```

  Echo's default error handler receives the returned error and attempts to write its own JSON error body, producing a double-write. `net/http` logs `http: superfluous response.WriteHeader call from ...` and the client sees the first response plus extra bytes, which is harmless but noisy and can confuse clients or proxies. The same file returns a similar wrapped error from other points (`authenticate`) where the response has not yet been written, so callers cannot tell the two cases apart.

- **Suggested fix**: Return `nil` after a failed `Upgrade` (log the error instead) so Echo does not attempt to send a second response. Leave the other error returns in `HandleWebSocket` untouched — they run before any response is sent.

---

## IH-007: `TripHandler.GetStats` and `GetTripByRoute` reject every session-authenticated request with 403

- **Severity**: high
- **Category**: logic
- **Location**: `internal/api/trip.go:87-93` (`GetStats`) and `internal/api/trip.go:170-176` (`GetTripByRoute`)
- **Status**: open
- **Description**: Both handlers hand-roll a per-device authorization check that only considers the JWT case and never looks at `ContextKeyAuthMode`:

  ```go
  authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
  if authDongleID != dongleID {
      return c.JSON(http.StatusForbidden, errorResponse{
          Error: "dongle_id does not match authenticated device",
          Code:  http.StatusForbidden,
      })
  }
  ```

  In `cmd/server/main.go:146` and `:178` these handlers are registered on groups gated by `sessionOrJWT` (`v1Routes` and `v1ConfigRead`). The session middleware (`internal/api/middleware/session.go:82-86`) stamps `ContextKeyAuthMode = "session"` but does *not* set `ContextKeyDongleID`, so for every dashboard request `authDongleID` is the empty string. Because `"" != "abc123"` is always true for any real dongle_id, the handler returns `403 Forbidden` to every session-authenticated caller.

  Concretely:
  - `web/src/app/page.tsx:193` fetches `/v1/devices/{dongleId}/stats` for the dashboard-home totals + recent-drives card, which now always renders the "Failed to load stats" error state.
  - `/v1/routes/{dongleId}/{routeName}/trip` is similarly unreachable from any session-authenticated page.

  The sibling handlers in the same package (e.g. `EventsHandler.ListEvents`, `RouteHandler.GetRoute`, `ExportHandler.ExportRouteGPX`, `ShareHandler.CreateShare`, `ConfigHandler` reads and writes) all funnel through the shared `checkDongleAccess(c, dongleID)` helper in `internal/api/auth_check.go`, which short-circuits when `mode == middleware.AuthModeSession`. `TripHandler` predates that helper and was never migrated; the unit tests in `trip_test.go:413` inject `ContextKeyDongleID` directly so they never exercise the session path.

- **Suggested fix**: Replace both inline checks in `trip.go` with `if handled, err := checkDongleAccess(c, dongleID); handled { return err }`, matching the rest of `internal/api/*`. Add a test case that sets `ContextKeyAuthMode = middleware.AuthModeSession` (with `ContextKeyDongleID` unset) and asserts the handler proceeds to 200.

---

## IH-008: `SignalsHandler.GetRouteSignals` rejects every session-authenticated request with 403

- **Severity**: high
- **Category**: logic
- **Location**: `internal/api/signals.go:81-87`
- **Status**: open
- **Description**: Same pattern as IH-007 in a different handler:

  ```go
  authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
  if authDongleID != dongleID {
      return c.JSON(http.StatusForbidden, errorResponse{
          Error: "dongle_id does not match authenticated device",
          Code:  http.StatusForbidden,
      })
  }
  ```

  The endpoint is registered in `cmd/server/main.go:131` under `v1Routes := e.Group("/v1/routes", sessionOrJWT)`, so dashboard sessions are expected to work. `web/src/components/video/SignalTimeline.tsx:119` fetches `/v1/routes/{dongleId}/{routeName}/signals` via `apiFetch`, which sends the browser's session cookie (`credentials: "include"` in `web/src/lib/api.ts:66`) but no Authorization header. Because `ContextKeyDongleID` is only populated by the JWT branch of the middleware, session requests fall into `authDongleID == ""`, which never matches the URL's dongle_id, and the route-detail page's signal timeline is unreachable.

  A byproduct of fixing IH-007 by routing through `checkDongleAccess` is that the same migration can be applied here for free.

- **Suggested fix**: Swap the hand-rolled check for `if handled, err := checkDongleAccess(c, dongleID); handled { return err }`. Optionally audit the package once more: `internal/api/signals.go`, `internal/api/trip.go`, and `internal/api/device_live.go`/`snapshot.go` are the only remaining files that inspect `ContextKeyDongleID` directly. `DeviceLiveHandler.GetLive` does no dongle check at all (it authorizes implicitly via the hub lookup), which is a separate, lower-severity consideration not tracked here.
