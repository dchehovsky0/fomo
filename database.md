# База записи fomo

PostgreSQL, база `fomo` в Docker: `postgres://postgres:test@localhost:55432/fomo`.

Время везде `timestamptz` (UTC). Сеть Solana: `network_id = 1399811149`.

Запись шла двумя кусками: 17:06–18:02 и 18:26–19:03 (Москва). Между ними и после 19:03 HTTP не ходил, дыры нельзя читать как «тезисов не было». Снимки трендов по WebSocket дописывались ещё какое-то время после истечения токена, уже без тезисов и метрик.

История тезисов по токену неполная. Из ленты сохраняются до 250 последних тезисов (10 страниц), из трендов — около 25 (1 страница). `history_complete = true` значит, что API сказал `hasNextPage = false`. Число строк в `theses` при этом чуть меньше `thesis_count`: часть элементов страницы не имеет тип `thesis`.

`v_thesis_timeline.n = 1` для токена с `history_complete = false` — это самый старый **сохранённый** тезис, а не первый тезис токена на fomo.

---

## runs

Один запуск `fomobot -record`.

| Поле | Тип | Смысл |
|---|---|---|
| `id` | bigint | Номер запуска. Сейчас 1 и 2. |
| `started_at` | timestamptz | Старт процесса. |
| `stopped_at` | timestamptz | Конец полезной записи. У запуска 2 пусто: процесс остановили по токену, не штатно. |
| `config` | jsonb | Кусок настроек без секретов: `recorder`, `network_id`, `feed_limit`, `rate_limit_rps`, `session_mode`. |

## tokens

Один токен — одна строка. Ключ `token_address` (mint Solana).

| Поле | Тип | Смысл |
|---|---|---|
| `token_address` | text | Mint. |
| `network_id` | text | `1399811149`. |
| `ticker` | text | Тикер. Пустой, если токен видели только в трендах и тезис по нему не скачался. |
| `image_url` | text | Картинка токена из ленты. |
| `first_seen_at` | timestamptz | Когда запись впервые увидела токен. |
| `first_seen_source` | text | `startup_feed` — был в ленте на старте процесса. `feed` — тезис появился в ленте уже во время работы. `trending` — впервые пришёл из WebSocket трендов. |
| `first_seen_run` | bigint | `runs.id` первого обнаружения. |
| `first_feed_thesis_at` | timestamptz | Когда по токену впервые увидели тезис, созданный уже после старта записи. Пусто, если в живой ленте таких тезисов не было. |
| `first_thesis_at` | timestamptz | Самый старый тезис, до которого дочитали. Точная дата рождения обсуждения на fomo только при `history_complete`. |
| `history_complete` | boolean | Дошли до конца пагинации `/feed/token/thesis`. |
| `thesis_count` | integer | Последний `count` из `/feed/token/thesis`. Это все тезисы токена за всё время, не число строк в `theses`. |
| `thesis_count_at_discovery` | integer | `count` в первый раз, когда его спросили. |
| `count_checked_at` | timestamptz | Время последней проверки `count`. |
| `details` | jsonb | Последний ответ `/proxy/tokenDetails`. Поля те же, что у снимка `source = tokenDetails`, см. ниже. |
| `details_at` | timestamptz | Когда снят `details`. |
| `last_activity_at` | timestamptz | Последняя активность: новый тезис в ленте или повторное появление. По ней решается, следить ли за токеном дальше. |
| `updated_at` | timestamptz | Любое последнее изменение строки. |

## users

Автор тезиса. Ключ `user_id` (UUID fomo). В текущей базе `verified` и `twitter` пустые у всех строк: API их не отдал.

| Поле | Тип | Смысл |
|---|---|---|
| `user_id` | text | Id автора. |
| `handle` | text | `userHandle`. |
| `display_name` | text | Отображаемое имя. Часто совпадает с handle. |
| `verified` | boolean | Флаг верификации с последнего тезиса. |
| `twitter` | jsonb | Объект twitter, если API его прислал. |
| `image_url` | text | Аватар. |
| `first_seen_at` | timestamptz | Первый тезис этого автора в нашей записи. |
| `last_seen_at` | timestamptz | Последний увиденный тезис автора. |

## theses

Последнее известное состояние одного тезиса. Строка из глобальной ленты и строка из `/feed/token/thesis` склеиваются по `thesis_id` (`t2-...`). Ключ `thesis_id`.

Колонки ниже — разобранные поля. Всё, что API прислал сверх них, лежит в `raw_feed` и `raw_token`.

| Поле | Тип | Смысл |
|---|---|---|
| `thesis_id` | text | Id тезиса, `t2-<hash>-<suffix>`. В ленте это `body.commentId`, не id события. |
| `token_address` | text | Mint. |
| `network_id` | text | Сеть. |
| `ticker` | text | Тикер на момент наблюдения. |
| `user_id` | text | Автор, ссылка на `users.user_id` логическая, внешнего ключа нет. |
| `user_handle` | text | Handle автора. |
| `display_name` | text | Имя автора. |
| `verified` | boolean | Автор верифицирован. |
| `is_dev` | boolean | Автор — создатель токена. |
| `twitter` | jsonb | Twitter автора, как пришёл в элементе. |
| `created_at` | timestamptz | Время создания тезиса на fomo, не время нашей записи. |
| `trade_id` | text | Сделка, к которой привязан тезис. |
| `swap_id` | text | Связанный swap. Обычно пусто, есть только у события ленты. |
| `transfer_id` | text | Связанный transfer. Обычно пусто. |
| `feed_event_id` | text | UUID события `/feed`, если тезис видели в глобальной ленте. |
| `comment` | text | Текст тезиса. |
| `price_usd_at_creation` | float | Цена токена в момент написания тезиса. |
| `market_cap_at_creation` | float | Капитализация в момент написания. |
| `fdv_at_creation` | float | FDV в момент написания. |
| `first_seen_at` | timestamptz | Когда мы впервые записали этот тезис. |
| `first_seen_source` | text | `feed` или `token_thesis` — откуда пришло первое наблюдение. |
| `seen_in_feed_at` | timestamptz | Когда тезис впервые попал в глобальную ленту. Пусто — в ленте его не было, только в истории токена. |
| `position_usd` | float | Текущая позиция автора в $, последнее наблюдение. Из ленты это `body.positionNotionalUsd`, из истории токена — `authorTrade.usdValue`. |
| `human_token_amount` | float | Сколько токенов у автора, последнее наблюдение. |
| `realized_pnl_usd` | float | Реализованный PnL, $. |
| `unrealized_pnl_usd` | float | Нереализованный PnL, $. |
| `pct_realized_pnl` | float | Реализованный PnL, %. В API поле `percentageRealizedPnl`. |
| `pct_unrealized_pnl` | float | Нереализованный PnL, %. |
| `position_closed_at` | timestamptz | Когда автор закрыл позицию (`authorTrade.closedAt`). Пусто — позиция ещё открыта или закрытие не приходило. Обновляется только из `/feed/token/thesis`. |
| `equity` | float | Поле `equity` элемента истории токена. |
| `threshold` | float | Поле `threshold` элемента. По живым данным близко к `position_usd`, смысл на стороне fomo до конца не ясен. |
| `likes` | integer | Лайки. Из ленты `likes`, из истории токена `comment.numLikes`. |
| `views` | integer | Просмотры. Есть только у события ленты. |
| `num_replies` | integer | Число ответов. |
| `older_thesis` | integer | Сколько более старых тезисов по токену видит fomo (`olderThesis`). |
| `newer_thesis` | integer | Сколько более новых. |
| `token_market_cap` | float | Капитализация токена на момент последнего наблюдения из глобальной ленты. У тезисов только из истории токена пусто. |
| `token_price` | float | Цена на момент последнего наблюдения из ленты. |
| `token_fdv` | float | FDV на момент последнего наблюдения из ленты. |
| `updated_at` | timestamptz | Время последнего наблюдения, которое изменило строку. |
| `raw_feed` | jsonb | Сырое событие `/feed`. Пусто, если тезис в глобальную ленту не попадал. |
| `raw_token` | jsonb | Сырой элемент `/feed/token/thesis`. |

Индексы: `(token_address, created_at)`, `(user_id)`.

### raw_feed — событие `/feed`

Верхний уровень:

| Ключ | Смысл |
|---|---|
| `id` | UUID события. Это `theses.feed_event_id`, не id тезиса. |
| `type` | `thesis_created`. Другие типы (например закреплённый `manual`) лежат в `feed_events`, в `theses` не попадают. |
| `userId` | Автор. |
| `tradeId`, `swapId`, `transferId` | Связанные сущности. |
| `tokenAddress`, `networkId` | Токен и сеть. |
| `createdAt` | Время события. |
| `verified` | Автор верифицирован. |
| `twitter` | Twitter автора или `null`. |
| `likes`, `views`, `numReplies` | Соцметрики события. |
| `pinned` | Закреплено. |
| `reacted` | Реакция текущего аккаунта. |
| `badge` | Бейдж, если есть. |
| `body` | Объект ниже. |
| `tradeComment` | Объект комментария, поля почти как `raw_token.comment`. |

`body`:

| Ключ | Смысл |
|---|---|
| `ticker` | Тикер. |
| `comment` | Текст. |
| `commentId` | Id тезиса `t2-...`. |
| `userId`, `userHandle`, `displayName`, `userImageUrl` | Автор. |
| `tokenAddress`, `networkId`, `tokenImageUrl` | Токен. |
| `isDev` | Автор — создатель токена. |
| `positionNotionalUsd` | Позиция автора, $. |
| `humanTokenAmount` | Количество токенов у автора. |
| `marketCap`, `price`, `fdv` | Текущие метрики токена в момент события ленты. |
| `marketCapAtCreation`, `priceUsdAtCreation`, `fdvAtCreation` | Метрики на момент тезиса. |
| `realizedPnlUsd`, `unrealizedPnlUsd` | PnL, $. |
| `percentageRealizedPnl`, `percentageUnrealizedPnl` | PnL, %. |
| `tag` | Метка, часто `null`. |
| `shortCommentSegments` | Массив `{text, link, provider}`. |

### raw_token — элемент `/feed/token/thesis`

| Ключ | Смысл |
|---|---|
| `type` | `thesis`. |
| `id` | Id тезиса `t2-...`. |
| `tradeId` | Сделка. |
| `createdAt` | Время тезиса. |
| `userId`, `displayName`, `userHandle`, `profilePictureLink` | Автор. |
| `verified`, `twitter`, `isDev` | Флаги автора. |
| `numReplies` | Ответы. |
| `tokenAddress`, `networkId`, `ticker`, `tokenImageUrl` | Токен. |
| `equity` | Поле fomo, в колонке `equity`. |
| `threshold` | Поле fomo, в колонке `threshold`. |
| `badge` | Бейдж. |
| `comment` | Объект ниже. |
| `authorTrade` | Объект ниже. Нет, если сделки автора в ответе не было. |

`comment`:

| Ключ | Смысл |
|---|---|
| `id`, `userId`, `tradeId` | Те же id. |
| `comment` | Текст. |
| `priceUsdAtCreation`, `marketCapAtCreation`, `fdvAtCreation` | Метрики на момент тезиса. |
| `createdAt` | Время комментария. |
| `parentId` | Родитель, если это ответ. |
| `numLikes` | Лайки. |
| `tokenAddress`, `networkId` | Токен. |
| `shortCommentSegments` | Массив `{text, link, provider}`. |
| `reactions` | `{counts: {likeCount}, reactions: {like}}`. `reactions.like` — реакция текущего аккаунта. |
| `olderThesis`, `newerThesis` | Сколько тезисов старше и новее. |

`authorTrade`:

| Ключ | Смысл |
|---|---|
| `humanTokenAmount` | Количество токенов. |
| `usdValue` | Позиция, $. |
| `unrealizedPnlUsd`, `realizedPnlUsd` | PnL, $. |
| `percentageUnrealizedPnl`, `percentageRealizedPnl` | PnL, %. |
| `closedAt` | Время закрытия позиции или `null`. |

## thesis_observations

Каждое изменившееся состояние тезиса. Новая строка пишется, только если изменились позиция, количество токенов, PnL, капитализация, цена, лайки, просмотры, ответы, `olderThesis`/`newerThesis`, время закрытия, handle или `verified`. Повтор того же состояния не пишется.

| Поле | Тип | Смысл |
|---|---|---|
| `id` | bigint | Синтетический ключ. |
| `thesis_id` | text | Тезис. |
| `token_address` | text | Токен. |
| `observed_at` | timestamptz | Когда мы это увидели. |
| `source` | text | `feed` или `token_thesis`. |
| `position_usd` | float | Позиция автора, $. |
| `human_token_amount` | float | Количество токенов. |
| `realized_pnl_usd` | float | Реализованный PnL. |
| `unrealized_pnl_usd` | float | Нереализованный PnL. |
| `position_closed_at` | timestamptz | Закрытие позиции, если уже было. |
| `token_market_cap` | float | Капитализация токена. Заполнена у источника `feed`. |
| `token_price` | float | Цена токена. Заполнена у источника `feed`. |
| `likes`, `views`, `num_replies` | integer | Соцметрики на этот момент. |
| `raw` | jsonb | Сырой объект того источника. У `feed` это всё событие, у `token_thesis` — элемент истории. |

Индексы: `(thesis_id, observed_at)`, `(token_address, observed_at)`.

## feed_events

Каждое событие глобальной ленты, включая не-тезисы. Ключ `event_id`. Повтор того же id не перезаписывается.

| Поле | Тип | Смысл |
|---|---|---|
| `event_id` | text | UUID события `/feed`. |
| `type` | text | `thesis_created` и прочие типы, которые лента отдала. |
| `token_address` | text | Mint, если есть. |
| `network_id` | text | Сеть. |
| `thesis_id` | text | `t2-...`, если событие — тезис. |
| `created_at` | timestamptz | Время события на fomo. |
| `first_seen_at` | timestamptz | Когда мы его впервые записали. |
| `raw` | jsonb | Событие целиком, та же форма, что `theses.raw_feed`. |

Индекс: `(token_address, created_at)`.

## thesis_counts

Как рос `count` токена.

| Поле | Тип | Смысл |
|---|---|---|
| `id` | bigint | Синтетический ключ. |
| `token_address` | text | Mint. |
| `observed_at` | timestamptz | Когда спросили. |
| `count` | integer | `count` ответа. Не зависит от параметра `threshold`. |
| `has_next_page` | boolean | Была ли следующая страница на первом запросе этой проверки. |
| `reason` | text | `history` — первая выгрузка страниц. `refresh` — перепроверка первой страницы. |

Индекс: `(token_address, observed_at)`.

## token_snapshots

Метрики токена во времени. Одна строка — один ответ по одному токену.

| Поле | Тип | Смысл |
|---|---|---|
| `id` | bigint | Синтетический ключ. |
| `token_address` | text | Mint. |
| `observed_at` | timestamptz | Когда снят снимок. Раз в минуту для `filterTokens`, раз в 30 минут для `tokenDetails`. |
| `source` | text | `filterTokens` или `tokenDetails`. Формат разный, см. ниже. |
| `data` | jsonb | Кусок ответа, относящийся к этому токену. |

Индекс: `(token_address, observed_at)`.

Числа в `filterTokens` часто приходят строками (`"24735597.17"`). В SQL приводить явно: `(data->>'marketCap')::float`.

### data при source = filterTokens

Ответ `/proxy/filterTokens`, один элемент на токен. Набор ключей плавает: у части снимков нет `change5m`, `volume5m`, `buyCount*` и `pinyin`.

Верхний уровень:

| Ключ | Смысл |
|---|---|
| `priceUSD` | Цена. |
| `marketCap` | Капитализация. |
| `liquidity` | Ликвидность, $. |
| `holders` | Число холдеров. |
| `createdAt` | Создание токена, unix-секунды. `to_timestamp((data->>'createdAt')::bigint)`. |
| `change5m`, `change1`, `change4`, `change12`, `change24` | Изменение цены за 5 минут, 1, 4, 12, 24 часа. Доля, не проценты: `0.31` = +31%. |
| `volume5m`, `volume1`, `volume4`, `volume12`, `volume24` | Объём за те же окна, $. |
| `txnCount1`, `txnCount4`, `txnCount12`, `txnCount24` | Число сделок. |
| `buyCount1`, `buyCount4`, `buyCount12`, `buyCount24` | Покупки. Часто `null`. |
| `sellCount1`, `sellCount4`, `sellCount12`, `sellCount24` | Продажи. Часто `null`. |
| `uniqueBuys1`, `uniqueBuys4`, `uniqueBuys12`, `uniqueBuys24` | Уникальные покупатели. Часто `null`. |
| `uniqueSells1`, `uniqueSells4`, `uniqueSells12`, `uniqueSells24` | Уникальные продавцы. Часто `null`. |
| `top10HoldersPercent` | Доля топ-10 холдеров. Часто `null` здесь и заполнена в `tokenDetails`. |
| `isHiddenFromDiscovery` | Скрыт из поиска fomo. |
| `pinyin` | Служебное поле, бывает не у всех. |
| `token` | Объект токена, ниже. |
| `pair` | `{protocol}`. Пример: `RaydiumCpmm`. |
| `exchanges` | Массив `{name}`. Пример: `Raydium CPMM`. |

`token`:

| Ключ | Смысл |
|---|---|
| `address` | Mint. |
| `networkId` | Сеть. |
| `name`, `symbol` | Имя и тикер. |
| `decimals` | Знаки. |
| `createdAt` | Создание, unix-секунды. Дубль верхнего `createdAt`. |
| `isScam` | Флаг скама, часто `null`. |
| `mintable`, `freezable` | Можно ли допечатать и заморозить. Часто `null`. |
| `i18n` | Локализация, часто `null`. |
| `info` | Объект ниже. |
| `launchpad` | Объект ниже. Иногда приходит не объектом. |
| `socialLinks` | `{twitter, telegram, website, discord}`. Пустые ссылки — `null`. |

`token.info`:

| Ключ | Смысл |
|---|---|
| `id` | `<mint>:1399811149`. |
| `address`, `networkId`, `name`, `symbol` | Дубли. |
| `description` | Описание токена. |
| `totalSupply`, `circulatingSupply` | Предложение. |
| `cmcId` | Id CoinMarketCap, часто `null`. |
| `imageThumbUrl`, `imageSmallUrl`, `imageLargeUrl`, `imageBannerUrl` | Картинки. |

`token.launchpad` (когда это объект):

| Ключ | Смысл |
|---|---|
| `launchpadName` | Площадка выпуска. Пример: `StonkFun`. |
| `launchpadIconUrl` | Иконка площадки. |
| `migrated` | Токен мигрировал с лаунчпада. |
| `graduationPercent` | Процент «выпускного», 100 — мигрировал. |

### data при source = tokenDetails

Ответ `/proxy/tokenDetails`. Это не паспорт токена, а срез торгов и холдеров. Тот же объект продублирован в `tokens.details`.

| Ключ | Смысл |
|---|---|
| `holders` | Холдеры. |
| `top10HoldersPercent` | Доля топ-10. |
| `isLowFees` | Флаг низких комиссий. |
| `buyCount5m`, `buyCount1`, `buyCount4`, `buyCount24` | Число покупок. |
| `sellCount5m`, `sellCount1`, `sellCount4`, `sellCount24` | Число продаж. |
| `buyVolume5m`, `buyVolume1`, `buyVolume4`, `buyVolume24` | Объём покупок, $. |
| `sellVolume5m`, `sellVolume1`, `sellVolume4`, `sellVolume24` | Объём продаж, $. |
| `uniqueBuys5m`, `uniqueBuys1`, `uniqueBuys4`, `uniqueBuys24` | Уникальные покупатели. |
| `uniqueSells5m`, `uniqueSells1`, `uniqueSells4`, `uniqueSells24` | Уникальные продавцы. |

Окна: `5m` — 5 минут, `1` — 1 час, `4` — 4 часа, `24` — 24 часа.

## api_responses

Целиком ответы, которые не разрезались по токенам. Сейчас это эталонные ответы `filterTokens` (по одному на запуск).

| Поле | Тип | Смысл |
|---|---|---|
| `id` | bigint | Синтетический ключ. |
| `observed_at` | timestamptz | Когда получен. |
| `endpoint` | text | `filterTokens`. |
| `request` | jsonb | Что отправили: массив `<mint>:1399811149`. |
| `response` | jsonb | Тело `responseObject` целиком. |

## trending_snapshots

Снимок потока трендов раз в 30 секунд. В `raw` лежит **последнее** WebSocket-сообщение окна, а не полный список. Полный набор mint'ов за окно — в `trending_sightings`.

| Поле | Тип | Смысл |
|---|---|---|
| `id` | bigint | Синтетический ключ. |
| `received_at` | timestamptz | Конец 30-секундного окна. |
| `token_count` | integer | Сколько разных Solana-mint попало в окно. |
| `raw` | jsonb | Последнее сообщение. Форма: `{type, topicType, topicId, payload}`. |

`payload` у живых сообщений — инкремент, не список целиком: `kind` (`update`), `index`, `update.token` (кусок того же объекта, что в `filterTokens`: `address`, `symbol`, `name`, `info`, `launchpad`). `index` — позиция в апдейте fomo. Это не колонка `trending_sightings.position`.

## trending_sightings

Какие токены встретились в окне трендов.

| Поле | Тип | Смысл |
|---|---|---|
| `snapshot_id` | bigint | Ссылка на `trending_snapshots.id`. |
| `token_address` | text | Mint. |
| `position` | integer | Порядок, в котором mint впервые встретился в этом 30-секундном окне, начиная с 1. Это не место в рейтинге fomo. |
| `observed_at` | timestamptz | То же время, что `trending_snapshots.received_at`. |

Индекс: `(token_address, observed_at)`.

## ws_messages

Служебные сообщения WebSocket и сообщения, в которых не нашли Solana-mint. Одинаковый `type` пишется не чаще раза в 10 секунд.

| Поле | Тип | Смысл |
|---|---|---|
| `id` | bigint | Синтетический ключ. |
| `received_at` | timestamptz | Когда пришло. |
| `type` | text | `challenge`, `challengeAccepted`, `subscribed`, `data`. |
| `raw` | jsonb | Сообщение целиком. |

## api_calls

Журнал HTTP-запросов к fomo. Нужен, чтобы видеть дыры, 429 и 430.

| Поле | Тип | Смысл |
|---|---|---|
| `id` | bigint | Синтетический ключ. |
| `run_id` | bigint | Запуск. |
| `at` | timestamptz | Начало запроса. |
| `method` | text | `GET` или `POST`. |
| `path` | text | `/feed`, `/feed/token/thesis`, `/proxy/filterTokens`, `/proxy/tokenDetails`. |
| `query` | text | Query-string. Для `/feed/token/thesis` здесь mint. |
| `status` | integer | HTTP-статус. `304` на `/feed` — лента не изменилась. Пусто, если ответа не было. |
| `duration_ms` | integer | Длительность. |
| `bytes` | integer | Размер тела ответа. |
| `error` | text | Текст ошибки транспорта, если статус не получен. |

Индекс: `(at)`.

---

## Представления

### v_new_tokens

Токены, чей первый тезис появился не раньше чем за 2 минуты до старта того запуска, который их обнаружил, и у которых `history_complete`. Колонки те же, что у `tokens`. Сейчас пусто: такие токены в записи не встречались.

### v_thesis_timeline

Тезисы токена по времени.

| Поле | Тип | Смысл |
|---|---|---|
| `token_address` | text | Mint. |
| `ticker` | text | Тикер тезиса. |
| `n` | bigint | Номер тезиса внутри токена, с 1. Для неполной истории это номер среди сохранённых, не среди всех тезисов fomo. |
| `created_at` | timestamptz | Время тезиса. |
| `since_first` | interval | Насколько этот тезис позже первого в этой нумерации. |
| `thesis_id` | text | Id тезиса. |
| `user_handle` | text | Автор. |
| `verified` | boolean | Флаг автора. |
| `is_dev` | boolean | Автор — создатель токена. |
| `position_usd` | float | Последняя известная позиция автора. |
| `market_cap_at_creation` | float | Капитализация в момент тезиса. |
| `token_market_cap` | float | Капитализация на последнем наблюдении из ленты. |
| `history_complete` | boolean | Можно ли считать `n = 1` первым тезисом токена. |
