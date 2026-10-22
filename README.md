# amneziawg-keenos-auto-connector

Сервис на Go для управления доменной маршрутизацией (`object-group fqdn` +
`dns-proxy route`) и AmneziaWG‑подключениями на роутерах Keenetic (KeeneticOS 5.x).

Проект ведётся по [OpenSpec](https://github.com/Fission-AI/OpenSpec): описание
сервиса и план этапов — в [`openspec/project.md`](openspec/project.md),
спецификации — в `openspec/specs/`, предложения — в `openspec/changes/`.

**Статус:** все шесть этапов (клиент роутера, группы и синхронизация, журнал и
уведомления, импорт AmneziaWG, REST API и веб‑интерфейс, свой сервер
AmneziaWG и выдача ключей) реализованы и заархивированы в `openspec/specs/`.

## Что умеет сейчас

* Транспорты **RCI** (HTTP‑API KeeneticOS, challenge‑response авторизация) и **SSH**.
* Чтение running‑config и разбор групп `object-group fqdn` и маршрутов `dns-proxy route`.
* План изменений как **дифф** «роутер → желаемое состояние»: только недостающие /
  лишние `include`/`exclude`/маршруты, без полной перезаписи групп.
* **Dry‑run**: `router plan` и `router apply --dry-run` ничего не отправляют.
* Применение с проверкой (состояние перечитывается после записи) и
  `system configuration save`.
* Бэкап running‑config перед каждым изменением, таймауты, ретраи транспортных
  ошибок, блокировка от параллельных запусков (в процессе + flock).
* **Группы доменов** из источников: URL‑списки community‑репозиториев и локальные
  файлы в форматах plain / hosts / dnsmasq / v2fly / Clash (автоопределение
  построчно), ручные `include` / `remove` / `exclude`, кэш загрузок с условным
  GET и откатом к последней удачной копии.
* **Подключения**: группа ссылается на `connection` (AWG, Reality …), маршруты
  `dns-proxy route` описаны один раз на подключение.
* **Нормализация**: регистр, IDN → punycode, проверка формата, дедупликация,
  схлопывание поддоменов, лимит записей (`error` или `truncate`).
* **Версии и откат**: каждая изменившаяся сборка — новая версия в `state_dir`;
  `groups rollback` закрепляет группу на прошлой версии и применяет дифф.
* **Защиты**: упавший источник не опустошает группу; резкое сокращение
  (`max_shrink`) блокируется без `--force`; dry‑run не пишет ни на роутер, ни в состояние.
* **Расписание**: `awgkeen serve` синхронизирует по cron, `awgkeen sync` — вручную.
* **Журнал изменений**: каждая синхронизация, откат, ручная правка и
  `router apply` (в том числе dry‑run) — запись в `state_dir/journal.jsonl`
  с результатом, версиями групп, `+N/-M`, числом команд, бэкапом и диффом;
  `awgkeen journal list|show` с фильтрами.
* **Уведомления** в Telegram и webhook (JSON, подпись HMAC‑SHA256): роутер
  недоступен, синхронизация не удалась, AWG упал. Одно сообщение на инцидент,
  напоминание раз в `repeat_interval`, сообщение о восстановлении; состояние
  алертов общее для демона и ручных запусков.
* **Проверки здоровья** в `serve`: доступность роутера, встроенная проверка
  AWG‑туннелей (интерфейс поднят, свежий handshake) и хуки‑команды
  (`health.checks`).
* **Импорт AmneziaWG**: `.conf` или ключ `vpn://` из приложения Amnezia →
  интерфейс `Wireguard<N>` на роутере (ключи, адрес, MTU, endpoint,
  AllowedIPs, keepalive, обфускация Jc/Jmin/Jmax/S1/S2/H1–H4 и AWG 2.0
  S3/S4/I1–I5). Идемпотентно и через дифф, как группы; ключи нигде не
  выводятся; чужие интерфейсы не трогаются. Маршруты групп могут ссылаться
  на туннель по имени (`tunnel: main`).
* **REST API и веб‑интерфейс** в `awgkeen serve` (`api.listen`): группы,
  версии, ручные правки, откат, синхронизация, журнал, алерты, проверки,
  туннели, состояние роутера, `/metrics` для Prometheus. Bearer‑токены
  (полный и только чтение), без токена — только localhost.
* **Свой сервер AmneziaWG без приложения Amnezia**: VPS по SSH или эта же
  машина. Установка amneziawg (Ubuntu), ключи и параметры обфускации, NAT,
  выдача клиентов одной командой — `.conf`, QR‑код и ключ `vpn://` для
  AmneziaVPN; отключение, ротация ключей, отзыв; изменения пиров без разрыва
  соединений; состояние и алерт `server_down`. Выданный конфиг сразу
  импортируется на роутер.

## Установка

```sh
go install github.com/DrCuper/amneziawg-keenos-auto-connector/cmd/awgkeen@latest
# или
make build && ./bin/awgkeen --help
```

## Конфигурация

Скопируйте [`config.example.yaml`](config.example.yaml) в `config.yaml`.
Секреты можно не писать в файл:

```sh
export AWGKEEN_ROUTER_PASSWORD='...'      # пароль admin (rci и ssh)
export AWGKEEN_ROUTER_KEY_FILE=~/.ssh/kn  # ключ для ssh
```

Желаемое состояние групп — отдельный YAML, см. [`examples/groups.yaml`](examples/groups.yaml):

```yaml
groups:
  - name: awg-main
    include: [example.com, api.example.com]
    exclude: [static.example.com]
    routes:
      - interface: Wireguard0
```

Описание групп, подключений и источников — `domains.yaml`,
см. [`examples/domains.yaml`](examples/domains.yaml):

```yaml
connections:
  awg:
    routes: [{interface: Wireguard0}]
groups:
  - name: awg-main
    connection: awg
    sources:
      - url: https://raw.githubusercontent.com/.../domains.lst
      - file: lists/extra.txt
    include: [example.com]
    remove:  [tracker.example.com]
```

## Команды

```sh
awgkeen sync                              # собрать группы из источников и применить дифф
awgkeen sync --dry-run                    # показать сводку и план, ничего не менять
awgkeen sync --group awg-main --force     # одна группа, без защиты от сокращения
awgkeen serve                             # демон: cron, проверки, импорт туннелей, REST API и веб-интерфейс

awgkeen groups list                       # версии, применённые версии, закрепления
awgkeen groups show awg-main [--version 3]
awgkeen groups history awg-main
awgkeen groups add awg-main site.example  # ручная запись без правки YAML (--apply — сразу на роутер)
awgkeen groups remove awg-main bad.example
awgkeen groups clear awg-main
awgkeen groups rollback awg-main [--to 3] # закрепить на прошлой версии и применить
awgkeen groups unpin awg-main             # снова собирать из источников

awgkeen journal list                      # последние 20 операций
awgkeen journal list -g awg-main --since 7d --real
awgkeen journal list --status failed --json
awgkeen journal show 42                   # запись целиком, с диффом
awgkeen journal prune                     # ротация по keep_days / max_entries

awgkeen notify test                       # тестовое сообщение во все каналы
awgkeen notify status                     # активные алерты
awgkeen health [--notify]                 # разовый прогон проверок

awgkeen awg inspect main.conf             # разобрать .conf / vpn:// без секретов (роутер не нужен)
awgkeen awg list                          # туннели и их интерфейсы
awgkeen awg plan                          # что изменится на роутере
awgkeen awg import nl 'vpn://...'         # ключ от уже работающего сервера → новый туннель
awgkeen awg apply [main] [--force-keys]   # создать/обновить Wireguard-интерфейс, проверить, сохранить
awgkeen awg status                        # интерфейс, пиры, handshake, трафик
awgkeen awg remove main                   # удалить интерфейс туннеля (только созданный awgkeen)

awgkeen server init|install|deploy        # свой сервер AmneziaWG (см. раздел ниже)
awgkeen server list|plan|status
awgkeen peer add phone --qr               # выдать ключ: .conf / --qr / --vpn / --png FILE
awgkeen peer list|show|rotate|disable|enable|remove NAME

awgkeen router show                       # группы и маршруты на роутере
awgkeen router show --raw                 # весь running-config
awgkeen router plan  -d groups.yaml       # что изменится (роутер не трогается)
awgkeen router apply -d groups.yaml --dry-run
awgkeen router apply -d groups.yaml       # применить, проверить, сохранить
awgkeen router apply -d groups.yaml --no-save
awgkeen router save                       # system configuration save
```

Пример вывода `plan`:

```
group awg-main:
  + include api.example.com
  - include old.example.com
  + route Wireguard0 auto

3 command(s):
  object-group fqdn awg-main include api.example.com
  no object-group fqdn awg-main include old.example.com
  dns-proxy route object-group awg-main Wireguard0 auto
```

Пример вывода `sync`:

```
group awg-main -> awg: version 7 (new), 1843 entries, 2 exclude, +12/-3 vs applied v6, collapsed 41
  source community: 1790 valid of 1802 lines, 12 skipped
  source local: 55 valid of 55 lines
group reality -> reality: version 2, 120 entries, +0/-0 vs applied v2

group awg-main:
  + include new.example.com
  ...
15 command(s):
  ...
applied 15 command(s), saved=true, backup=/var/lib/awgkeen/backups/running-config-20261021-120000.txt
```

Пример `journal list`:

```
ID  TIME                 KIND    TRIGGER   MODE     STATUS  GROUPS                  SUMMARY
14  2026-10-21 12:00:03  sync    schedule  real     ok      awg-main v7 +12/-3      15 command(s) applied, saved
13  2026-10-21 11:58:40  manual  cli       real     ok      awg-main                add site.example
12  2026-10-21 06:00:02  sync    schedule  real     failed  awg-main v6             apply: running-config failed after 4 attempt(s): …
```

## Уведомления

Каналы и секреты — в секции `notify` (см. [`config.example.yaml`](config.example.yaml)):

```sh
export AWGKEEN_TELEGRAM_BOT_TOKEN='123456:ABC...'   # токен бота от @BotFather
export AWGKEEN_TELEGRAM_CHAT_ID='123456789'
export AWGKEEN_WEBHOOK_URL='https://example.com/hooks/awgkeen'
export AWGKEEN_WEBHOOK_SECRET='...'                 # подпись X-Awgkeen-Signature: sha256=<hmac>
awgkeen notify test
```

| Алерт | Когда срабатывает | Когда снимается |
|---|---|---|
| `router_unreachable` | роутер не ответил после всех ретраев (sync или проверка `health`) | роутер ответил |
| `sync_failed` | роутер ответил, но синхронизация не удалась (отказ команды, верификация, ошибка группы) | успешная синхронизация |
| `awg_down:<tunnel>` | туннель `awg.tunnels`: интерфейса нет, он выключен или handshake старше `check.max_handshake_age` | свежий handshake |
| `awg_down:<check>` | хук `health.checks` вернул ненулевой код или превысил таймаут | хук вернул 0 |

Dry‑run и ручные правки алерты не меняют. Тело webhook:

```json
{"event":"firing","alert":"router_unreachable","key":"router_unreachable","title":"Router unreachable",
 "text":"apply: running-config failed after 4 attempt(s): …","since":"2026-10-21T06:00:02Z","failures":3,
 "time":"2026-10-21T07:00:02Z","host":"nas"}
```

`event`: `firing`, `reminder`, `resolved`, `info` (при `on_apply: true`), `test`.

## AmneziaWG‑туннели

Проще всего — импортировать ключ от уже работающего сервера (из приложения
AmneziaVPN, от друга, провайдера или выданный `awgkeen peer add`), без правки
конфига. Каждый ключ — отдельный туннель и свой `Wireguard<N>`, так что
серверов может быть сколько угодно:

```sh
awgkeen awg import nl 'vpn://AAAF...'      # ключ из AmneziaVPN («Поделиться» → ключ)
awgkeen awg import de /path/to/de.conf     # или .conf; `-` — из stdin
awgkeen awg import nl new.conf --replace   # сервер перевыпустил ключ
awgkeen awg remove de                      # удалить интерфейс и забыть ключ
```

Ключи хранятся в `state_dir/tunnels/<имя>.conf` (0600). То же — во вкладке
«Туннели» веб‑интерфейса и через `POST /api/v1/tunnels/import`
(`{"name":"nl","key":"vpn://…"}`). Группы направляются на туннель по имени,
несколько маршрутов подряд дают резерв:

```yaml
connections:
  vpn:
    routes: [{tunnel: nl}, {tunnel: de}]
```

Туннели можно описать и в `config.yaml` (с тонкими настройками):

```yaml
awg:
  tunnels:
    - name: main
      conf_file: /etc/awgkeen/main.conf   # .conf или файл с vpn://... (chmod 600)
      interface: auto                     # или Wireguard2; auto = свободный номер, запоминается
      # ip_global: auto                   # «использовать для выхода в интернет» (по умолчанию не трогаем)
```

и в `domains.yaml` маршрут на туннель по имени:

```yaml
connections:
  awg:
    routes: [{tunnel: main}]
```

```
$ awgkeen awg plan
tunnel main -> Wireguard1 (create)
  warning: DNS from the config is ignored: the router keeps its own DNS (domain routing relies on it)

interface Wireguard1:
  + interface Wireguard1
  ~ description "awgkeen: main"
  ~ ip address 10.8.1.2/32
  ~ private-key <secret>
  ~ asc 4 40 70 0 0 1 2 3 4
  + peer QkJCQkJC…
  ~ peer QkJCQkJC… endpoint vpn.example.com:51820
  + peer QkJCQkJC… allow-ips 0.0.0.0/0
  + up
```

Ключи роутер в running‑config не показывает, поэтому они отправляются при
создании интерфейса, при смене ключей в `.conf` (сервис хранит отпечаток в
`state_dir/awg.json`) или по `--force-keys`. Интерфейс, созданный не сервисом,
меняется только с `adopt: true`. DNS из `.conf`, IPv6 и параметры AWG 1.5
J1–J3/Itime игнорируются с предупреждением. `awgkeen serve` импортирует
туннели при старте (`awg.apply_on_start`) и проверяет их каждые
`health.interval`.

## Свой сервер AmneziaWG

Сервер и ключи к нему — без приложения AmneziaVPN. Нужен VPS (Ubuntu 22.04/24.04
для автоматической установки) с доступом root по SSH или пользователь с
`sudo` без пароля; UDP‑порт `listen_port` должен быть открыт.

```yaml
servers:
  - name: vps
    host: 203.0.113.10
    key_file: /etc/awgkeen/vps_ed25519
    known_hosts_file: /etc/awgkeen/known_hosts   # ssh-keyscan 203.0.113.10 > known_hosts
    endpoint: 203.0.113.10                        # или DNS-имя; клиенты подключаются сюда
```

```sh
awgkeen server init                 # ключи, адрес 10.8.1.1, параметры Jc/Jmin/Jmax/S1/S2/H1–H4
awgkeen server install              # amneziawg из PPA Amnezia (один раз)
awgkeen server deploy               # конфиг, NAT, ip_forward, awg-quick@awg0
awgkeen peer add phone --qr         # новый клиент: QR для приложения AmneziaWG
awgkeen peer add laptop > laptop.conf
awgkeen peer show phone --vpn       # ключ vpn:// для AmneziaVPN
awgkeen peer show phone --png phone.png
awgkeen peer add keenetic > /etc/awgkeen/main.conf   # и в awg.tunnels: conf_file этого файла
awgkeen peer list                   # адреса, handshake, трафик
awgkeen peer rotate phone           # новые ключи, старый конфиг перестаёт работать
awgkeen peer disable phone          # временно заблокировать; enable — вернуть
awgkeen peer remove phone           # отозвать навсегда
awgkeen server plan | status        # что изменится на хосте / состояние интерфейса
```

`peer add/remove/rotate/disable/enable` сразу разворачивают конфиг
(`--no-deploy` — только локально). Изменения клиентов применяются через
`awg syncconf` без разрыва остальных; смена ключей или параметров сервера
(`server init --force`) перезапускает интерфейс и требует перевыпуска всех
клиентов. Ключи хранятся в `state_dir/servers/<name>.json` (0600), чтобы
конфиг можно было выдать повторно. При нескольких серверах — `--server NAME`.
`local: true` управляет машиной, где запущен `awgkeen`. Если сервер не
поднят, `serve` шлёт алерт `server_down:<name>`. Все операции — в журнале.

## REST API и веб‑интерфейс

```yaml
api:
  listen: 192.168.1.10:8080       # пусто = выключено; без токена — только 127.0.0.1/::1
  # token / read_token — или AWGKEEN_API_TOKEN / AWGKEEN_API_READ_TOKEN, или *_file
```

Веб‑интерфейс — `http://192.168.1.10:8080/` (токен вводится справа сверху и
хранится в браузере). Примеры API:

```sh
H="Authorization: Bearer $AWGKEEN_API_TOKEN"
curl -H "$H" http://host:8080/api/v1/status
curl -H "$H" http://host:8080/api/v1/groups
curl -H "$H" -X POST http://host:8080/api/v1/sync -d '{"dry_run":true}'
curl -H "$H" -X POST http://host:8080/api/v1/groups/awg-main/manual -d '{"add":["site.example"],"apply":true}'
curl -H "$H" -X POST http://host:8080/api/v1/groups/awg-main/rollback -d '{"to":3}'
curl -H "$H" 'http://host:8080/api/v1/journal?group=awg-main&since=7d'
curl -H "$H" http://host:8080/api/v1/tunnels/status
curl -H "$H" http://host:8080/metrics
```

| Метод и путь | Что делает |
|---|---|
| `GET /api/v1/health` | живость, без токена |
| `GET /api/v1/status` | сводка: группы, туннели, алерты, последняя синхронизация, проверки |
| `GET /api/v1/groups`, `/groups/{name}[?version=N]`, `/groups/{name}/versions` | группы, записи версии, история |
| `POST /api/v1/groups/{name}/manual` | `{"add":[],"remove":[],"clear":false,"apply":false}` |
| `POST /api/v1/groups/{name}/rollback`, `/unpin` | `{"to":N,"dry_run":false}`, `{"apply":false}` |
| `POST /api/v1/sync` | `{"groups":[],"dry_run":false,"force":false}` |
| `GET /api/v1/journal[?group&kind&status&since&dry_run&limit]`, `/journal/{id}` | журнал |
| `GET /api/v1/alerts`, `/checks`, `/router` | алерты, проверки, состояние роутера (без ключей) |
| `GET /api/v1/tunnels`, `/tunnels/status`; `POST /api/v1/tunnels/apply` | AWG‑туннели |
| `POST /api/v1/tunnels/import`; `DELETE /api/v1/tunnels/{name}` | `{"name","key","apply":true,"replace":false}` — добавить ключ любого сервера; удалить импортированный |
| `GET /api/v1/servers`, `/servers/{s}/status`, `/servers/{s}/peers` | серверы, состояние, клиенты (без ключей) |
| `POST /api/v1/servers/{s}/peers` | `{"name":"phone","deploy":true}` — выдать ключ |
| `POST /api/v1/servers/{s}/peers/{p}/rotate`, `/disable`, `/enable`; `DELETE /api/v1/servers/{s}/peers/{p}` | операции с клиентом (`{"deploy":false}` — без деплоя) |
| `POST /api/v1/servers/{s}/deploy` | `{"dry_run":false}` |
| `GET /api/v1/servers/{s}/peers/{p}/conf`, `/qr.png`, `/vpn` | конфиг клиента — только полный токен |
| `GET /metrics` | Prometheus |

Изменяющие запросы, cron и импорт при старте выполняются по одному: если
что‑то уже идёт, API отвечает `409`. Ответ операций — запись журнала
(`entry`), сводка (`summary`) и план (`plan`, ключи — `<secret>`).

## Как это работает

Оба транспорта исполняют **одни и те же CLI‑строки** KeeneticOS
(RCI — через `POST /rci/ [{"parse": "..."}]`, SSH — через exec), а состояние
читается из running‑config. JSON‑запись в ветки RCI не используется намеренно:
неверное поле там — тихий no‑op. После применения состояние перечитывается;
если расхождение осталось, возвращается ошибка верификации и конфиг не сохраняется.

Синхронизация (`sync`/`serve`): загрузить `domains.yaml` → для каждой группы
скачать источники (или взять закреплённую версию) → разобрать, нормализовать,
применить `remove`/лимит → сравнить хэш с последней версией и при изменении
сохранить новую → собрать `Desired` из всех групп и маршрутов их подключений →
`Client.Apply` (lock, бэкап, дифф, проверка, save) → запомнить применённые версии.

```
cmd/awgkeen/          CLI
internal/app/         сборка конфиг → транспорт → клиент → store → syncer
internal/config/      YAML + env
internal/domains/     domains.yaml, нормализация, сборка группы
internal/source/      загрузка списков (HTTP с кэшем, файлы) и парсер форматов
internal/store/       файловое состояние: версии, applied/pinned, ручные правки, кэш
internal/syncer/      оркестратор синхронизации, откат, планировщик cron, наблюдатели
internal/journal/     журнал изменений (JSON Lines), запросы, ротация
internal/notify/      Telegram/webhook, алерты с подавлением повторов и восстановлением
internal/health/      проверки здоровья: роутер, AWG-туннели, хуки-команды; монитор для serve
internal/awg/         разбор .conf / vpn://, импорт туннелей, оценка handshake
internal/api/         REST API, метрики, встроенный веб-интерфейс (web/index.html)
internal/server/      свой сервер AmneziaWG: ключи, пиры, рендер, деплой, статус, vpn:// и QR
internal/remote/      выполнение скриптов локально и по SSH
internal/router/      модель, план (дифф), Client: lock → read → backup → plan → apply → verify → save
internal/router/cli/  парсер running-config и генерация команд
internal/router/rci/  транспорт RCI
internal/router/sshx/ транспорт SSH
internal/router/fake/ фейковый роутер для тестов
internal/lock/        flock
```

## Разработка

```sh
make test   # go test -race ./...
make lint   # golangci-lint
openspec validate --strict
```

Тесты не требуют сети, реального роутера и VPS.

## Дорожная карта

Все этапы из [`openspec/project.md`](openspec/project.md) реализованы. Не
проверено на реальном оборудовании: применение на роутере (этапы 1–5) и
развёртывание на настоящем VPS (этап 6).
