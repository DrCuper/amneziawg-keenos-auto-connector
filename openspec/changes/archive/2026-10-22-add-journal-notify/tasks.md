# Tasks

## 1. Точка расширения в syncer

- [x] 1.1 `internal/syncer`: `Observer`, `Report{Kind, Trigger, DryRun, Started, Duration, Result, Err, Manual}`, `Options.Trigger`; вызов наблюдателей из `Run`, `Rollback` и ручных правок; `Scheduler` передаёт `schedule`/`startup`; тесты

## 2. Журнал

- [x] 2.1 `internal/journal`: `Entry` и построение из `syncer.Report` и `router.ApplyResult` (статусы ok/partial/failed/skipped, `changed`, лимит строк диффа); тесты по сценариям спеки
- [x] 2.2 `internal/journal`: файловое хранилище JSON Lines — добавление под flock, счётчик номеров, чтение с пропуском битых строк, `Query` с фильтрами, `Prune` (keep_days, max_entries), автоматическая ротация по размеру; тесты (параллельная запись, битая строка, ротация)

## 3. Уведомления

- [x] 3.1 `internal/notify`: каналы Telegram и webhook (подпись HMAC, повторы 5xx/429, без повтора 4xx, очистка секретов из ошибок); тесты на `httptest.Server`
- [x] 3.2 `internal/notify`: `Alerter` — состояние алертов в `alerts.json` под flock, порог, подавление, напоминания, `resolved`, повтор недоставленного `firing`; классификация `syncer.Report`; `on_apply`; тесты по сценариям спеки

## 4. Проверки здоровья

- [x] 4.1 `internal/health`: интерфейс `Check`, проверка роутера, хук‑команда (без shell, таймаут, хвост вывода), `Monitor` с интервалом; тесты

## 5. Конфиг, CLI, документация

- [x] 5.1 `internal/config`: секции `journal`, `notify`, `health`; секреты из env и `*_file`; валидация; тесты
- [x] 5.2 `internal/app`: сборка журнала, `Alerter`, проверок; наблюдатели в `NewSyncer`
- [x] 5.3 `cmd/awgkeen`: `journal list|show|prune`, `notify test|status`, `health`; журнал для `groups add|remove|clear|unpin` и `router apply`; `serve` запускает проверки и ежедневную ротацию
- [x] 5.4 README, `config.example.yaml`, `openspec/project.md`; `openspec validate --strict`, `make test lint`
