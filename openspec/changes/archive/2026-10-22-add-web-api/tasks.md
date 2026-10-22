# Tasks

## 1. API

- [x] 1.1 `internal/api`: сервер, Bearer‑авторизация (полный и read‑only токены), заголовки безопасности, лимит тела, JSON‑ошибки
- [x] 1.2 Группы: список, группа с версией, история; ручные правки, откат, открепление с `apply`
- [x] 1.3 Синхронизация с dry‑run/force; ответ = запись журнала + сводка + план; 422/409
- [x] 1.4 Журнал (фильтры, запись), алерты, проверки, `/status`, состояние роутера без ключей
- [x] 1.5 Туннели: список, состояние, план/импорт с записью в журнал
- [x] 1.6 `/metrics` (Prometheus text)
- [x] 1.7 Тесты: авторизация, поток туннель → синхронизация → правка → откат → журнал, 409, ошибки запросов, метрики, UI

## 2. Интеграция

- [x] 2.1 `syncer`: `TriggerAPI`, `DefaultTrigger`; `health.Monitor.OnResults`; `journal.ParseSince`
- [x] 2.2 `config`: секция `api`, токены из env/файлов, валидация loopback/длины/TLS, маскирование
- [x] 2.3 `app.NewAPI`; `serve`: HTTP‑сервер с graceful shutdown, общий мьютекс операций для cron, старта и API

## 3. Веб‑интерфейс

- [x] 3.1 `internal/api/web/index.html`: обзор, группы, журнал, туннели, роутер; вывод только текстом; подтверждения
- [x] 3.2 Проверка в headless Chromium без ошибок JS

## 4. Документация

- [x] 4.1 README, `config.example.yaml`, `openspec/project.md`; `openspec validate --strict`, `go vet`, `go test -race`, `golangci-lint`
