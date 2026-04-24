-- Route annotations: free-form note, starred flag, and many-per-route tags.
-- These columns back the user-editable metadata surfaced by the web UI.

ALTER TABLE routes
    ADD COLUMN note    TEXT    NOT NULL DEFAULT '',
    ADD COLUMN starred BOOLEAN NOT NULL DEFAULT false;

-- Partial index keeps the starred-only filter fast without bloating the index
-- with rows that are never starred (mirrors idx_routes_preserved).
CREATE INDEX idx_routes_starred ON routes(dongle_id) WHERE starred = true;

-- Tags are shared across a device's tag catalog (many-to-many style, but keyed
-- only by (route_id, tag) so the set of distinct tags per device is derived by
-- joining back to routes). Tags are normalized to lowercased, trimmed form and
-- bounded to a reasonable length to keep the catalog tidy.
CREATE TABLE route_tags (
    route_id INT  NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    tag      TEXT NOT NULL
        CHECK (tag = lower(trim(tag)) AND length(tag) BETWEEN 1 AND 32),
    PRIMARY KEY (route_id, tag)
);

-- Supports listing which tags exist per device via a join to routes, and
-- powers tag-based filters on the route list.
CREATE INDEX idx_route_tags_tag ON route_tags(tag);
