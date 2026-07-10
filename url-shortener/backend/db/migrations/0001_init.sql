CREATE TABLE IF NOT EXISTS links (
    code        TEXT    PRIMARY KEY,
    url         TEXT    NOT NULL,
    visit_count INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_links_created_at ON links (created_at DESC);
