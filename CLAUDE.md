# music_release_notifier — Release Radar

Telegram-бот, що сповіщає про нові релізи артистів, на яких підписався користувач.
Плюс Spicetify extension як другий вхід для підписки (v2).

**Мова: Go. БД: PostgreSQL. Джерело релізів: ListenBrainz / MusicBrainz.**

---

## ⚠️ Прочитай перед будь-якою порадою

**`SPEC.md` у корені — джерело правди.** Там ~35 обмежень, перевірених емпірично
(живі запити до API, читання встановленого Spotify-клієнта, вихідники Spicetify).
Не переказуй їх з пам'яті — відкрий файл.

**Твої тренувальні дані про Spotify API застаріли.** Spotify зробив три раунди
breaking changes: листопад 2024, лютий 2026, липень 2026. Майже кожен туторіал,
блогпост і SO-відповідь про Spotify Web API зараз неправильні.

### П'ять помилок, які ти зробиш за замовчуванням

1. **Не пропонуй Spotify Web API.** Client Credentials з лютого 2026 вимагає
   **активного Premium власника app**. У автора Premium немає і не буде. Джерело
   релізів — ListenBrainz. Spotify у бекенді немає взагалі. *(C12, C13)*
2. **Telegram-бот не може написати за `@username`.** Лише числовий `chat_id`, і
   лише після того, як користувач сам натиснув Start. *(C1, C2)*
3. **`days=N` у ListenBrainz — це вікно ±N днів**, а не «останні N». Перевірено:
   `days=3` → 7 днів і 29 майбутніх релізів. Використовуємо
   `days=7&past=true&future=false`. *(C24, D15)*
4. **`data-testid` на сторінці артиста Spotify не існує** — клієнт вирізає назву
   атрибута. Не пиши селектори з ним. *(C18)*
5. **Не використовуй `Spicetify.CosmosAsync`** для запитів на наш бекенд — він
   тихо проксує через сторонній Cloudflare Worker і викидає headers. Тільки
   звичайний `fetch()`. *(C17)*

---

## Архітектура

Один Go-бінарник, чотири воркери в горутинах. **Не мікросервіси** — розділення
логічне, не мережеве.

| Воркер | Відповідальність |
|---|---|
| `bot` | Telegram long polling, команди, пошук артистів, callback-и, `my_chat_member` |
| `poller` | Раз на добу: ListenBrainz → матчинг проти `artists` → `releases` + `notifications` |
| `notifier` | Дренує outbox через `SKIP LOCKED`, тримає темп, ретраїть, класифікує помилки |
| `api` | **v2.** REST для Spicetify extension |

Postgres — окремий сервіс, не в тому ж контейнері.

## Порядок робіт

`S0` скелет → `S1` бот відповідає → `S2` пошук артиста → `S3` підписки →
`S4` детекція → `S5` доставка → `S6` продакшн → **v1 готова** →
`S8` API+лінкування → `S9` підписка з Spotify → `S10` публікація extension.

`S7` (спайк extension) робиться паралельно з `S1`. Деталі кожного етапу — `SPEC.md` §11.

---

## Інваріанти, які не можна порушувати

**Ідемпотентність — це обмеження БД, а не логіка в коді.**
Ніколи не пиши «перевір, чи є, потім вставляй» — це race condition. Пиши:

```sql
INSERT INTO notifications (user_id, release_id)
VALUES ($1, $2)
ON CONFLICT (user_id, release_id) DO NOTHING;
```

- `subscriptions PRIMARY KEY (user_id, artist_mbid)` — підписка з бота і з
  extension фізично не можуть створити дубль.
- `notifications UNIQUE (user_id, release_id)` — гарантія «одне повідомлення на
  один реліз», навіть якщо поллер відпрацює двічі.
- `releases UNIQUE (release_group_mbid)` — MusicBrainz release-*group* уже
  об'єднує всі видання одного альбому.

**Канал доставки — плагін.** Telegram-специфічні типи (`chat_id`, `parse_mode`,
`retry_after`, `retry` семантика) **не течуть** вище межі інтерфейсу `Notifier`.
Домен каже «сповісти користувача U про реліз R» і нічого не знає про Telegram.

**Черга — Postgres `SELECT … FOR UPDATE SKIP LOCKED`, не брокер.** Це усвідомлене
рішення (D8), не недогляд. RabbitMQ — окрема міграція у v2. Тримай чергу за
інтерфейсом, щоб заміна була локальною.

**Redis немає.** Кеш перед перевіркою дедуплікації вносить race condition і нічого
не пришвидшує. Постгрес — це і є кеш на цьому масштабі. *(D9, SPEC §8)*

---

## Ліміти, які треба дотримувати в коді

| Куди | Ліміт | Як |
|---|---|---|
| Telegram, один чат | ~1 msg/s | token bucket на `chat_id` |
| Telegram, глобально | ~30 msg/s → **беремо 25** | глобальний token bucket |
| Telegram 429 | `parameters.retry_after`, секунди | спати рівно стільки |
| MusicBrainz | **1 req/s на IP** + осмислений `User-Agent` | глобальний limiter |
| ListenBrainz | 1 запит на добу | і цього достатньо |

Обробка помилок Telegram: `403 bot was blocked` / `user is deactivated` /
`chat not found` — **постійні**, видаляй підписку. `429` / `5xx` — транзієнтні,
backoff. `400 can't parse entities` — наш баг, підписку **не чіпай**.
`401` — алярм, **ніколи** не видаляй підписки. Повна таблиця — `SPEC.md` §10.

---

## Технологічні рішення

- **Telegram-бібліотека: `mymmrac/telego`.** Найпопулярніша
  `go-telegram-bot-api` **мертва** (останній коміт жовтень 2022, Bot API ~6.0).
- **`parse_mode=HTML`, не MarkdownV2.** MarkdownV2 вимагає екранування 18
  символів за трьома контекстними правилами; у stdlib є `html.EscapeString`.
  MarkdownV2 вб'ється на `Panic! At The Disco`.
- **Long polling, не webhook.** Telegram сам це радить; працює за NAT без домену.
- **Пікер артиста: одна картка з пагінацією** (`sendPhoto` + `editMessageMedia`),
  не 5 окремих повідомлень. `sendMediaGroup` **не має** `reply_markup`.
- **`callback_data` = 64 байти.** Імена артистів не влазять — зберігай кандидатів
  на сервері, шли `s:7fa3:2`.
- **`answerCallbackQuery` викликай ПЕРШИМ**, до роботи — інакше в клієнта висить
  спінер (~15 s дедлайн).

## Стиль коду

- Стандартна бібліотека там, де вона достатня. Без веб-фреймворку — `net/http`.
- `pgx` для Postgres. Явний SQL, без ORM.
- `log/slog`, структурні логи.
- `context.Context` першим аргументом у все, що робить I/O.
- Помилки обгортати з контекстом: `fmt.Errorf("poll listenbrainz: %w", err)`.
- Міграції — впорядковані `.sql` файли, застосовуються при старті.
- Тести на інваріанти дедуплікації **обов'язкові** — це те, що ламається тихо.

---

## Процес — ніяких прямих пушів у main

**Кожна зміна йде через гілку й PR.** `main` не приймає прямих комітів.

```bash
git checkout -b feat/s2-artist-search   # feat/ fix/ chore/ docs/ + номер етапу
make check                             # те саме, що перевіряє CI
git push -u origin HEAD
gh pr create --fill                    # шаблон у .github/pull_request_template.md
```

Опис PR мусить відповідати на питання, якого **не видно з дифу**: що розглядалося
й було відкинуто, яке рішення зі `SPEC.md` це реалізує. «Added artist search» —
поганий опис; «MusicBrainz замість Spotify, бо C12» — хороший.

## Лінтер і конвенції

```bash
make lint     # golangci-lint run
make check    # fmt + vet + lint + тести з БД, у порядку CI
```

- **`golangci-lint` запінений на `v2.13.2`** одночасно в `.golangci.yml` (коментар
  у шапці), у `env.GOLANGCI_LINT_VERSION` у `.github/workflows/ci.yml`, і локально
  (`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2`).
  Оновлювати **всі три разом**.
- **Чому саме v2, а не v1:** офіційні бінарники v1 зібрані під Go 1.24 і
  відмовляються працювати з модулем, що таргетить Go 1.26 — `can't load config:
  the Go language version (go1.24) used to build golangci-lint is lower than the
  targeted Go version (1.26.2)`. Пінити **номер версії недостатньо**: бінарник
  мусить бути зібраний тулчейном не старішим за директиву `go` в `go.mod`.
  Саме на цьому CI впав з першого разу, поки локально було зелено (локальний
  бінарник був самозібраний під go1.26).
- **Форматування — через `golangci-lint fmt`** (`gofmt` + `goimports` з
  `local-prefixes`), а не окремим кроком `gofmt` у CI. Одне джерело правди.
- **`depguard` машинно стежить за межею `Notifier`** (D13): будь-який імпорт
  `telego` поза `internal/telegram` — це помилка збірки в CI, а не коментар,
  який хтось прочитає. Перевірено негативним тестом.
- **Орфографія — US English** (`misspell locale: US`), як в екосистемі Go.
  Не `honoured`/`cancelled`/`serialise`, а `honored`/`canceled`/`serialize`.
- Заглушки лінтера **завжди з причиною**. `//nolint:xxx` без пояснення — це
  борг, а не рішення. Вимкнені перевірки (`shadow`, `hugeParam`, `fieldalignment`)
  задокументовані просто в `.golangci.yml`.
- `-race` у CI, але **не локально**: race-детектор потребує cgo, а стандартний
  Windows-тулчейн без C-компілятора його не має.

## Правило спеки

**`SPEC.md` оновлюється тим самим комітом, що й зміна в коді.**
Спека, яка відстала від коду, гірша за відсутність спеки — вона бреше з виглядом
істини. Кожна зміна рішення чи обмеження → рядок у §16 «Журнал змін».

## Робоче середовище

- Windows 11, PowerShell (є і Bash). Go 1.26.2, Docker 28.3.2, compose v2.39.1.
- **Порт 5432 зайнятий нативним Windows-сервісом PostgreSQL.** Контейнер БД
  замаплений на **5433** на хості. `localhost:5432` веде в чужий Postgres —
  не в наш. Усередині compose-мережі app звертається до `db:5432` як звичайно.
- Тести інваріантів вимагають `TEST_DATABASE_URL` (порт 5433). Без цієї змінної
  вони **скіпаються**, щоб `go test ./...` був зелений без Docker.
- Spicetify 2.44.0, Spotify 1.2.96.518.
- Extensions лежать у `%APPDATA%\spicetify\Extensions\` (**Roaming**, не Local —
  Local це каталог бінарника).
- Після правки extension: `spicetify apply` або `spicetify watch -e`.
  **Extension копіюється, не лінкується** — правка джерела без re-push не діє.
- Після **кожного** оновлення Spotify: `spicetify backup apply`.
