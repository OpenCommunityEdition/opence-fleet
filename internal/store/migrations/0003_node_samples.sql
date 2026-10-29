-- A node's health once a minute (its page's graphs), kept as long as the
-- servers' samples. cpu is the agent's share of the CPUs busy (0 to 100);
-- net_in and net_out are bytes a second, from the agent's counters. Each
-- is NULL when it is not known (an agent before them, a counter reset).
CREATE TABLE node_samples (
  node_id INTEGER NOT NULL,
  time INTEGER NOT NULL,
  cpu REAL,
  load REAL NOT NULL DEFAULT 0,
  memory_used INTEGER NOT NULL DEFAULT 0,
  memory_total INTEGER NOT NULL DEFAULT 0,
  disk_used INTEGER NOT NULL DEFAULT 0,
  disk_total INTEGER NOT NULL DEFAULT 0,
  net_in REAL,
  net_out REAL,
  PRIMARY KEY (node_id, time)
);
CREATE INDEX node_samples_time ON node_samples (time);
