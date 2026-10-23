# Tasks

- [x] 1.1 `awg import`, API и UI: по умолчанию без применения на роутере, флаг `--apply` / `apply`
- [x] 1.2 Разбор ключа администратора (`awg.DecodeVPNDocument`, `server.ParseAdminKey`), регистрация сервера (0600), TOFU host key
- [x] 1.3 Сервер Amnezia: поиск контейнера и конфига, чтение `[Interface]`, слияние пиров с пометкой, `syncconf` в контейнере, защита от смены ключей
- [x] 1.4 CLI `server import|remove`, `peer add --as-tunnel`; `server list` с импортированными
- [x] 1.5 API `KeyImport`, `DELETE /api/v1/servers/{s}`; UI: галочка «применить», «Забыть сервер»
- [x] 1.6 Тесты: фейковый хост Amnezia (адреса, деплой, удаление, смена ключей), ключ администратора, TOFU, реестр; проверка UI; README
