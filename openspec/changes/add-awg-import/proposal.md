# Proposal

## Why

Маршрутизация доменов (этапы 1–3) отправляет трафик в интерфейс `Wireguard<N>`,
но сам интерфейс оператор пока создаёт руками в веб‑интерфейсе роутера:
загружает `.conf` из приложения Amnezia, а для прошивок без поддержки ASC
вводит `wireguard asc …` в CLI. Ключи меняются при перевыпуске конфига, а
состояние туннеля проверяется только внешним хуком. Это этап 4 плана из
`openspec/project.md`.

## What Changes

- **Импорт AWG‑конфига** (`awg-connection`): в конфиге сервиса описываются
  туннели `awg.tunnels[]` — файл `.conf` Amnezia или файл с ключом `vpn://`
  (формат «Поделиться» приложения Amnezia). Сервис разбирает ключи, адрес,
  MTU, endpoint, AllowedIPs, keepalive и параметры обфускации
  Jc/Jmin/Jmax/S1/S2/H1–H4 (а также S3/S4/I1–I5 AmneziaWG 2.0) и приводит
  интерфейс `Wireguard<N>` на роутере к этому состоянию через тот же
  `router.Client`: план → бэкап → применение → проверка → сохранение.
- Интерфейс задаётся явно (`interface: Wireguard2`) или выбирается
  автоматически (`auto`, наименьший свободный номер) и запоминается в
  `state_dir/awg.json`. Чужой (созданный не сервисом) интерфейс не трогается
  без `adopt: true`; удаляются только свои интерфейсы.
- Ключи (private/preshared) роутер в running‑config не показывает, поэтому
  они отправляются при создании интерфейса, при смене ключей (по отпечатку в
  `awg.json`) и по `--force-keys`; в выводе плана, журнале, логах и ошибках
  ключи маскируются.
- **Маршруты на туннель**: в `domains.yaml` подключение может ссылаться на
  туннель по имени (`routes: [{tunnel: main}]`) — синхронизация подставит его
  интерфейс.
- **Встроенная проверка AWG**: для каждого туннеля `serve` проверяет, что
  интерфейс существует, поднят и имеет свежий handshake
  (`check.max_handshake_age`, 5 мин по умолчанию); сбой — алерт
  `awg_down:<tunnel>` этапа 3. Статус читается через `show interface`
  (RCI JSON или текст консоли по SSH).
- CLI: `awgkeen awg inspect|list|plan|apply|remove|status`; `serve` импортирует
  туннели при старте (`awg.apply_on_start`, по умолчанию да) до первой
  синхронизации. Каждое применение и удаление пишется в журнал
  (`awg-apply`, `awg-remove`).

Вне scope: DNS из `.conf` (роутер оставляет свой DNS — на нём держится
доменная маршрутизация), IPv6‑адреса и IPv6 AllowedIPs, параметры AWG 1.5
J1–J3/Itime (KeeneticOS их не поддерживает), управление сервером и выдача
конфигов (этап 6).

## Capabilities

### New Capabilities
- `awg-connection`: импорт клиентских конфигов AmneziaWG на роутер, привязка
  маршрутов к туннелям, проверка состояния туннеля.

### Modified Capabilities
(нет — `router-client` расширяется интерфейсами Wireguard без изменения
поведения для групп; алерт `awg_down` уже описан в `notifications`)

## Impact

- Новый пакет `internal/awg` (разбор `.conf`/`vpn://`, менеджер туннелей,
  оценка здоровья).
- `internal/router`: `State.Interfaces`, `Desired.Interfaces`/`RemoveInterfaces`,
  `WGStatus`, `Client.InterfaceStatus`; секреты в `Plan` маскируются;
  верификация игнорирует write‑only изменения.
- `internal/router/cli`: разбор секций `interface Wireguard<N>`, команды и дифф
  интерфейсов, разбор вывода `show`.
- Транспорты RCI/SSH/фейк: чтение статуса интерфейса.
- `internal/syncer`: разрешение `tunnel:` в маршрутах; `internal/health`:
  `AWGCheck`.
- Конфиг: секция `awg`. Зависимость `golang.org/x/crypto/curve25519` (уже в
  модуле `x/crypto`).
