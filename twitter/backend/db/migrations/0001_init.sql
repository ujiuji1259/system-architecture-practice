CREATE TABLE IF NOT EXISTS users (
    id             INTEGER PRIMARY KEY,          -- Snowflake id
    handle         TEXT NOT NULL UNIQUE,
    display_name   TEXT NOT NULL DEFAULT '',
    follower_count INTEGER NOT NULL DEFAULT 0,   -- denormalized, so the celebrity check is O(1)
    created_at     TEXT NOT NULL
);

-- The follow graph. fan-out needs "who follows X" (followers of an author),
-- paged by follower id, hence the (followee_id, follower_id) index.
CREATE TABLE IF NOT EXISTS follows (
    follower_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    followee_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (follower_id, followee_id)
);

CREATE INDEX IF NOT EXISTS idx_follows_followee ON follows (followee_id, follower_id);

-- Tweets. The id is a Snowflake, so ordering by id is chronological and the
-- home-timeline merge is a simple sort. The (author_id, id DESC) index serves
-- the pull path: a celebrity's recent tweets and a user's own timeline.
CREATE TABLE IF NOT EXISTS tweets (
    id         INTEGER PRIMARY KEY,
    author_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    text       TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tweets_author ON tweets (author_id, id DESC);
