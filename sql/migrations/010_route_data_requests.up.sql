-- Route data requests track on-demand pulls of full-resolution dashcam data
-- (HEVC video and full rlogs) that the device does not auto-upload by default.
--
-- requested_by stores the operator's username for session-authenticated
-- callers, or NULL for device JWT callers (we do not record a dongle id here
-- since route_id already pins the device).
CREATE TABLE route_data_requests (
    id              SERIAL PRIMARY KEY,
    route_id        INT NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    requested_by    TEXT,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind            TEXT NOT NULL CHECK (kind IN ('full_video', 'full_logs', 'all')),
    status          TEXT NOT NULL CHECK (status IN ('pending', 'dispatched', 'partial', 'complete', 'failed')) DEFAULT 'pending',
    dispatched_at   TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    error           TEXT,
    files_requested INT NOT NULL DEFAULT 0
);

-- Indexed lookups for the polling endpoint (latest by route+kind) and the
-- dispatcher worker (pending rows whose route/device may now be online).
CREATE INDEX idx_route_data_requests_route_id ON route_data_requests(route_id);
CREATE INDEX idx_route_data_requests_status ON route_data_requests(status)
    WHERE status = 'pending';
