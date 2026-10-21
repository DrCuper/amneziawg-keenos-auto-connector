## MODIFIED Requirements

### Requirement: Нормализация
Сервис SHALL нормализовать записи группы: нижний регистр, обрезка пробелов и
завершающей точки, IDN → punycode, проверка формата (домен или IP/CIDR,
как в `router-client`), дедупликация с сохранением первого вхождения.
При `collapse_subdomains: true` (по умолчанию) записи, чей родительский домен
уже есть в группе, SHALL удаляться. Записи `localhost`, `local`, `*`, IP‑адреса
`0.0.0.0`/`127.0.0.1`/`::1` SHALL отбрасываться. Имена без точки SHALL
отбрасываться из источников и SHALL сохраняться в ручных записях оператора.

#### Scenario: Дедупликация и регистр
- **WHEN** источники дают `Example.com`, `example.com.`, `EXAMPLE.COM`
- **THEN** в группе одна запись `example.com`

#### Scenario: Схлопывание поддоменов
- **WHEN** в группе есть `example.com` и `api.example.com`
- **THEN** остаётся только `example.com`

#### Scenario: IDN
- **WHEN** источник содержит `пример.рф`
- **THEN** в группе запись `xn--e1afmkfd.xn--p1ai`

#### Scenario: Имя без точки из источника
- **WHEN** источник содержит `com` или `twitch`
- **THEN** запись отбрасывается

#### Scenario: Имя без точки в ручном include
- **WHEN** в `include` группы указано `youtube`, а на роутере в группе уже есть `include youtube`
- **THEN** запись `youtube` остаётся в группе и `sync --dry-run` не планирует её удаление

#### Scenario: Зарезервированное имя в ручном include
- **WHEN** в `include` указано `localhost`
- **THEN** запись отбрасывается и учитывается как невалидная
