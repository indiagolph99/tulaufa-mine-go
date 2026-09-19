# tulaufa-mine-go

Управляющий демон для `minecraft.service` на сервере tulaufa.ru. Отдаёт статус,
старт/стоп/рестарт и живой лог в `/api/mc/*`; фронтенд живёт в репозитории
[tulaufa](https://github.com/indiagolph99/tulaufa) на маршруте `/minecraft-admin`.

Слушает только `127.0.0.1:8787` — наружу его пускает nginx.

## Зачем отдельный сервис

Сайт статический (`adapter-static`), серверной части у него нет. Демон на Go
занимает ~15 МБ RSS, а на сервере свободно около 485 МБ — Node-рантайм туда уже
не помещается комфортно.

## Устройство

```
cmd/tulaufa-mine/     точка входа, конфиг, подкоманда hash
internal/auth/        PBKDF2-хеш пароля, сессии, ограничение попыток входа
internal/mc/          вызов mc-ctl, разбор статуса, раздача логов подписчикам
internal/httpapi/     маршруты и middleware
deploy/               mc-ctl, unit, сниппет nginx, setup.sh
testdata/mc-ctl-stub  заглушка, чтобы всё работало без systemd
```

Зависимостей нет — только стандартная библиотека (`crypto/pbkdf2` появился в Go 1.24).

## Безопасность

- Демон не умеет ничего, кроме как запустить `/usr/local/bin/mc-ctl` через sudo.
  Скрипт принадлежит root, принимает ровно `status|start|stop|restart|logs-follow`
  и подставляет имя юнита сам — аргумент вызывающего никогда не доходит до
  `systemctl` или `journalctl`.
- Пароль хранится как PBKDF2-SHA256 (600 000 итераций) в `/etc/tulaufa-mine/env`.
- Сессия — случайные 32 байта в памяти; cookie `HttpOnly`, `Secure`, `SameSite=Strict`.
- Вход ограничен: 5 попыток в минуту с адреса.
- Запросы, меняющие состояние, требуют совпадения заголовка `Origin`.
- Действия дебаунсятся (10 с), чтобы двойной клик не дёргал JVM.
- Все входы и действия пишутся в journal вместе с IP.

## Разработка

Systemd не нужен — есть заглушка:

```sh
export MC_CTL="$PWD/testdata/mc-ctl-stub"
export ADMIN_PASSWORD_HASH="$(go run ./cmd/tulaufa-mine hash)"   # спросит пароль
export ALLOWED_ORIGIN=http://localhost:5173
export SECURE_COOKIE=false
go run ./cmd/tulaufa-mine
```

Тесты: `go test ./...`. Проверка: `go vet ./...`.

## Конфигурация

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `ADMIN_PASSWORD_HASH` | — (обязательна) | результат `tulaufa-mine hash` |
| `ALLOWED_ORIGIN` | `https://tulaufa.ru` | допустимый `Origin` |
| `LISTEN_ADDR` | `127.0.0.1:8787` | адрес прослушивания |
| `SESSION_TTL` | `12h` | время жизни сессии |
| `SECURE_COOKIE` | `true` | выключать только для localhost без TLS |
| `MC_CTL` | `sudo -n /usr/local/bin/mc-ctl` | команда-обёртка |
| `ACTION_DEBOUNCE` | `10s` | минимальный интервал между действиями |
| `MAX_LOG_STREAMS` | `4` | одновременных SSE-потоков |

## Установка на сервер

```sh
ssh -p <port> root@<host> 'bash -s' < deploy/setup.sh
```

Скрипт идемпотентен: создаёт пользователя `tulaufa-mine`, ставит `mc-ctl`,
проверяет sudoers через `visudo -c`, ставит и включает unit, а затем сам
проверяет, что обёртка работает и что мусорный аргумент отвергается.

Пароль и nginx — вручную, скрипт печатает инструкции в конце.

## Выкатка

Вручную: **Actions → deploy → Run workflow** с ветки `main`, затем подтвердить
гейт окружения `production`. Workflow собирает статический бинарник, копирует
его в `/opt/tulaufa-mine/`, перезапускает сервис, проверяет здоровье и ставит
тег `v<дата>`.

## Если забыт пароль

Сгенерировать новый хеш локально (`tulaufa-mine hash`), заменить строку в
`/etc/tulaufa-mine/env`, затем `systemctl restart tulaufa-mine.service`.
