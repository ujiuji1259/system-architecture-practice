CREATE TABLE IF NOT EXISTS links (
    code       TEXT PRIMARY KEY,
    url        TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_links_created_at ON links (created_at DESC);

-- One row per access. The visit count is derived by counting these rows,
-- so there is a single source of truth for "how many times a link was used".
CREATE TABLE IF NOT EXISTS link_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    code        TEXT NOT NULL REFERENCES links(code) ON DELETE CASCADE,
    accessed_at TEXT NOT NULL,
    referer     TEXT NOT NULL DEFAULT '',
    user_agent  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_link_events_code_time ON link_events (code, accessed_at DESC);
