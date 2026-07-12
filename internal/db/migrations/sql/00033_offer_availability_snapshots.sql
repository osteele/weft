-- +goose Up
CREATE TABLE IF NOT EXISTS offer_availability_snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at INTEGER NOT NULL,
    provider TEXT NOT NULL DEFAULT '',
    gpu_class TEXT NOT NULL DEFAULT '',
    gpu_mem_bucket_gb INTEGER NOT NULL DEFAULT 0,
    disk_bucket_gb INTEGER NOT NULL DEFAULT 0,
    num_gpus INTEGER NOT NULL DEFAULT 1,
    interconnect TEXT NOT NULL DEFAULT '',
    instance_type TEXT NOT NULL DEFAULT '',
    runpod_cloud_type TEXT NOT NULL DEFAULT '',
    min_reliability REAL NOT NULL DEFAULT 0,
    offer_count INTEGER NOT NULL DEFAULT 0,
    post_filter_count INTEGER,
    price_min_cents INTEGER,
    price_median_cents INTEGER,
    price_p75_cents INTEGER,
    details_json TEXT
);

CREATE INDEX IF NOT EXISTS idx_offer_availability_snapshots_bucket_time
    ON offer_availability_snapshots (
        provider,
        gpu_class,
        gpu_mem_bucket_gb,
        disk_bucket_gb,
        num_gpus,
        interconnect,
        instance_type,
        runpod_cloud_type,
        min_reliability,
        created_at
    );

-- +goose Down
DROP INDEX IF EXISTS idx_offer_availability_snapshots_bucket_time;
DROP TABLE IF EXISTS offer_availability_snapshots;
