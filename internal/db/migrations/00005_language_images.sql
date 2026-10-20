-- +goose Up

-- Песочница запускает решения в официальных образах тулчейнов: собственные образы
-- autocheck/sandbox-* не нужны, обновление компилятора — смена тега.
UPDATE languages SET image = 'golang:1.27-alpine'            WHERE id = 'go1.27';
UPDATE languages SET image = 'gcc:15'                        WHERE id = 'cpp20-gcc15';
UPDATE languages SET image = 'python:3.14-alpine'            WHERE id = 'python3.14';
UPDATE languages SET image = 'eclipse-temurin:25-jdk-alpine' WHERE id = 'java25';

-- +goose Down

UPDATE languages SET image = 'autocheck/sandbox-go:1.27'     WHERE id = 'go1.27';
UPDATE languages SET image = 'autocheck/sandbox-gcc:15'      WHERE id = 'cpp20-gcc15';
UPDATE languages SET image = 'autocheck/sandbox-python:3.14' WHERE id = 'python3.14';
UPDATE languages SET image = 'autocheck/sandbox-java:25'     WHERE id = 'java25';
