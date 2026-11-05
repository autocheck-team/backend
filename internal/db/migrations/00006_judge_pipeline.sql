-- +goose Up
ALTER TABLE problems ADD COLUMN stop_on_first_failure boolean NOT NULL DEFAULT false;
ALTER TABLE problems ADD COLUMN checker_ready boolean NOT NULL DEFAULT false;
ALTER TABLE test_runs ADD COLUMN cpu_time_ms integer NOT NULL DEFAULT 0;
ALTER TABLE languages ADD COLUMN compile_env text[] NOT NULL DEFAULT '{}';
-- Полный индекс не исключает аренды, оставленные упавшими воркерами.
DROP INDEX judge_jobs_ready_idx;
CREATE INDEX judge_jobs_ready_idx ON judge_jobs (run_after, locked_until);
UPDATE languages SET compile_env = ARRAY['GOCACHE=/work/box/.cache', 'GOTMPDIR=/work/box', 'CGO_ENABLED=0'] WHERE id = 'go1.27';
UPDATE languages SET compile_env = ARRAY['TMPDIR=/work/box'] WHERE id = 'cpp20-gcc15';
-- SerialGC и ограниченный heap оставляют память и потоки для самой JVM.
UPDATE languages SET run_cmd = ARRAY['java', '-XX:+UseSerialGC', '-XX:ActiveProcessorCount=1', '-XX:MaxRAMPercentage=50', '-Xss512k', 'Main'],
 compile_cmd = ARRAY['javac', '-J-XX:+UseSerialGC', '-J-XX:ActiveProcessorCount=1', 'Main.java'] WHERE id = 'java25';

-- +goose Down
UPDATE languages SET run_cmd = ARRAY['java', '-Xss64m', 'Main'], compile_cmd = ARRAY['javac', 'Main.java'] WHERE id = 'java25';
DROP INDEX judge_jobs_ready_idx;
CREATE INDEX judge_jobs_ready_idx ON judge_jobs (run_after) WHERE locked_by IS NULL;
ALTER TABLE languages DROP COLUMN compile_env;
ALTER TABLE test_runs DROP COLUMN cpu_time_ms;
ALTER TABLE problems DROP COLUMN checker_ready, DROP COLUMN stop_on_first_failure;
