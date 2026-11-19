# Проверка мониторинга HPRPR-15

Проверено 19 ноября 2026 на Docker Desktop arm64 и Go 1.27.4.

Стенд собран через Compose с абсолютным build context этого backend worktree для
api/worker/migrate и project name `hprpr15verify`. Временный override использовал
REST 18015, gRPC 19015, Keycloak 18115, S3 18315, PostgreSQL 55415,
Prometheus 19115, Grafana 13015, Loki 13115. Порт хоста 5432 не использовался.
В тестовом стенде платёжный провайдер был выключен; постоянные env не менялись.

## Сквозной прогон

Токены получены у собственного Keycloak через `autocheck-cli`. Через REST созданы
задание и тест, файлы загружены по presigned PUT в SeaweedFS, тест подтверждён,
задание опубликовано. Посылки прошли REST → gRPC → PostgreSQL/S3 → worker → Docker.

| Наблюдение | Результат |
| --- | --- |
| 11 посылок | 7 OK, по одному WA, RE, CE и SE |
| Watch через REST | Поток отдал result и завершился |
| Повторная компиляция на free | HTTP 429, счётчик compilation = 1 |
| Запрос без токена | HTTP 401 |
| Prometheus API `up` | api = 1, worker = 1 |
| Максимальная глубина очереди за прогон | waiting = 8, running = 2 |
| Метрики worker | attempts = 11, system_errors = 1, active в конце = 0 |
| Стадии | compile count = 2, run count = 9 |
| Уборка | workspace success = 10, sweep success = 1 |
| Grafana API | Dashboard uid autocheck, provisioned = true, 13 панелей |
| Запросы панелей | Все PromQL успешно выполнены через Prometheus API |
| Loki API | Логи api и worker, JSON-поля service/level/msg; все 11 ID присутствуют у обоих сервисов |
| Make monitoring-down / monitoring-up | Остановка сохранила работающее приложение; после запуска scrape и dashboard доступны |

SE получен на собственном неопубликованном задании без тестов: это постоянная
ошибка, повторов для неё нет. Retry и ошибки фиксации результата дополнительно
проверены unit-тестами. Счётчики платежей проверены через настоящий PostgreSQL и
REST/gRPC gateway с HTTP-стабом провайдера, включая параллельные повторы и webhook.

## Проверки кода

- `go build ./...`, `go vet ./...`, `make lint`: PASS, 0 замечаний линтера.
- `go test -race -count=1 ./...`: PASS, 383 успешных кейса, без пропусков DB/Docker-тестов.
- Пропущен только опциональный `TestYookassaLive`: ключи платёжного магазина не заданы.
- Покрытие: monitoring 77.4%, judge 79.8%, server 82.1%, billing 84.8%,
  submission 87.5%, config 82.4%.
- `govulncheck` v1.9.0: нет затронутых вызовов и импортируемых пакетов;
  одно предупреждение уровня зависимого модуля.
- gopls MCP требует approval при политике never: инструменты недоступны.
  Вместо диагностики gopls использованы build, vet, lint и тесты.
- Proto и миграции не менялись; proto-gen/proto-lint и проверка миграций не требовались.

После проверки Compose-проект удалён через `down -v`, включая его данные.
Временные override, инструменты и локальные проверочные образы удалены.
Чужие контейнеры и PostgreSQL на 5432 не изменялись.


## Изменённые файлы

Backend:

- `internal/monitoring/{metrics.go,interceptor.go,server.go,metrics_test.go}` — registry,
  экспортер, RED, контекст логов и тесты.
- `internal/judge/{monitoring.go,monitoring_test.go,worker.go,runner.go}` — снимок
  очереди, попытки, стадии, вердикты и уборка.
- `internal/billing/{repository.go,repository_test.go}`,
  `internal/submission/service.go` — платежи и отказы по квотам.
- `internal/server/{server.go,billing_test.go}` — подключение middleware и проверка
  метрик через настоящий gateway.
- `internal/config/{config.go,config_test.go}`, `cmd/{api,worker}/main.go` — env,
  слушатели, управление фоновыми задачами, service в JSON-логах.
- `go.mod`, `go.sum`, `configs/.env.example`, `README.md`, `docs/database.md`, этот отчёт.

Infra:

- `compose.yaml`, `Makefile`, `.env.example`, `README.md`.
- `deploy/monitoring/{prometheus.yaml,loki.yaml,config.alloy}`.
- `deploy/monitoring/grafana/provisioning/{datasources,dashboards}/autocheck.yaml`.
- `deploy/monitoring/grafana/dashboards/autocheck.json`.
