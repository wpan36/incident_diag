-- Dropped in dependency order: a table is dropped before the tables it
-- references, so no foreign key is ever left pointing at something gone.
DROP TABLE IF EXISTS evidence;
DROP TABLE IF EXISTS tool_calls;
DROP TABLE IF EXISTS agent_steps;
DROP TABLE IF EXISTS agent_runs;
DROP TABLE IF EXISTS incidents;
DROP TABLE IF EXISTS documents;
