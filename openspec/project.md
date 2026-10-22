# amneziawg-keenos-auto-connector

Сервис на Go для управления доменной маршрутизацией и AmneziaWG‑подключениями
на роутерах Keenetic (KeeneticOS 5.x). Один бинарник, конфиг в YAML, состояние
в файле/SQLite, dry‑run везде, идемпотентность по умолчанию.

## Назначение

1. **Списки доменов (ядро).** Источники: URL‑списки, локальные файлы, ручные
   include/exclude. Каждая группа = `object-group fqdn` на роутере, привязанная к
   своему подключению (`dns-proxy route object-group <group> <iface> auto`).
   Нормализация, дедупликация, лимиты. На роутер уходит только дифф.
   Расписание по cron, ручной запуск, откат к предыдущей версии списка.
2. **Клиент роутера.** SSH или RCI (HTTP JSON‑API KeeneticOS). Чтение
   running‑config, создание/обновление групп, привязка маршрутов, сохранение.
   Dry‑run, таймауты, ретраи, блокировка от параллельных изменений, бэкап
   конфигурации перед изменением.
3. **AWG‑подключение на роутере.** Импорт `.conf` Amnezia (ключи, endpoint,
   Jc/Jmin/Jmax/S1/S2/H1–H4), создание/обновление `Wireguard<N>`, проверка
   handshake/трафика/доступности endpoint.
4. **Управление сервером (позже).** Пиры AWG, выдача IP, клиентские `.conf`/QR,
   ротация ключей.
5. **Наблюдаемость и безопасность.** REST API/веб‑интерфейс, журнал изменений,
   метрики и алерты (Telegram/webhook), авторизация API, секреты вне репозитория.
6. **Эксплуатация.** Один бинарник / Docker‑образ, YAML‑конфиг, идемпотентность.

## Порядок реализации (каждый этап — отдельный OpenSpec change)

| # | Этап                                         | Change                    | Capability            |
|---|----------------------------------------------|---------------------------|-----------------------|
| 1 | Клиент роутера и dry‑run                     | `add-router-client`       | `router-client`       |
| 2 | Группы доменов, дифф‑синхронизация, cron     | `add-domain-groups-sync`  | `domain-groups`, `sync-scheduler` |
| 3 | Журнал и уведомления                         | `add-journal-notify`      | `change-journal`, `notifications` |
| 4 | Импорт AWG‑конфига на роутер                 | `add-awg-import`          | `awg-connection`      |
| 5 | Веб‑интерфейс / REST API                     | `add-web-api`             | `http-api`, `web-ui`  |
| 6 | Управление сервером и пирами                 | `add-server-peers`        | `awg-server`          |

## Архитектура

```
cmd/awgkeen/            CLI‑точка входа (router ..., sync, serve, groups ...)
internal/config/        YAML‑конфиг + переменные окружения для секретов
internal/domains/       domains.yaml (подключения, группы, источники), нормализация, сборка
internal/source/        загрузка списков (HTTP с кэшем, файлы), парсер форматов
internal/store/         состояние: версии групп, applied/pinned, ручные правки, кэш
internal/syncer/        оркестратор синхронизации, откат, планировщик cron, Observer
internal/journal/       журнал изменений (JSON Lines), запросы и ротация
internal/notify/        каналы Telegram/webhook, алерты с подавлением повторов
internal/health/        проверки здоровья (роутер, AWG, хуки) и монитор для serve
internal/awg/           разбор .conf/vpn://, импорт туннелей в Wireguard<N>, оценка handshake
internal/router/        Доменная модель роутера: State, Desired*, Plan, Client
internal/router/rci/    Транспорт RCI (HTTP, challenge‑response auth)
internal/router/sshx/   Транспорт SSH (CLI через exec)
internal/router/cli/    Генерация CLI‑команд и парсер running-config
internal/router/fake/   Фейковый транспорт для тестов
internal/lock/          Файловая блокировка (flock)
openspec/               Спецификации и change‑предложения
```

Принципы:

* **Транспорт = исполнение CLI‑строк.** И SSH, и RCI (`POST /rci/ [{"parse": "..."}]`)
  умеют выполнять команды CLI KeeneticOS. Состояние читается из
  `running-config` — одного и того же текста в обоих транспортах. Это даёт
  единый парсер, единый планировщик и тривиальный фейк для тестов.
* **План прежде применения.** Любая операция сначала строит `Plan`
  (список команд + человекочитаемый дифф). Dry‑run = показать план.
* **Идемпотентность.** План строится из разницы «текущее → желаемое»;
  пустой план ничего не делает и не сохраняет конфиг.
* **Секреты вне репозитория.** Пароль/ключ берутся из env
  (`AWGKEEN_ROUTER_PASSWORD`, `AWGKEEN_ROUTER_KEY_FILE`,
  `AWGKEEN_TELEGRAM_BOT_TOKEN`, `AWGKEEN_WEBHOOK_SECRET` …) или из файла,
  на который ссылается конфиг (`*_file`); в YAML их класть можно, но не нужно.
* **Наблюдатели.** `syncer` сообщает о каждой операции через `Observer`;
  журнал и уведомления подключаются в `internal/app`, не меняя `syncer`.

## Конвенции

* Go 1.24, `go vet` + `golangci-lint`, `make test` без сети.
* Тексты OpenSpec — на русском, ключевые слова спек (SHALL, WHEN/THEN) — на английском.
* Коммиты и PR — на английском, общение с заказчиком — на русском.
