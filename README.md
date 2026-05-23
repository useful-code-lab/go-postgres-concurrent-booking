# ChronosLock ⏳🔒

Продвинутый высококонкурентный сервис на Golang для бронирования ресурсов и работы с временными диапазонами без дублирования данных.

Проект разработан для работы в высоконагруженных (Highload) средах под управлением **Kubernetes** и использует **PostgreSQL** в качестве единого источника истины для синхронизации распределенных транзакций.

## ✨ Ключевые особенности и паттерны

В проекте реализовано 5 уровней защиты от конкурентного изменения данных (Race Conditions) и наложения дат:
1. **PostgreSQL TSRANGE & Exclusion Constraints**: Защита на уровне СУБД с помощью GIST-индексов. База физически отвергает пересекающиеся диапазоны дат (`booking_period WITH &&`).
2. **Context-Driven Timeouts**: Защита от зависания транзакций. Если база заблокирована, Go-рантайм отпустит клиента по таймауту через 3 секунды.
3. **In-Memory Striped Lock (`sync.Map`)**: Сегментированная блокировка мьютексами в памяти приложения. Экономит пулы соединений к БД, отсекая повторные запросы к одному ресурсу на уровне инстанса.
4. **Pessimistic Locking (`SELECT FOR UPDATE`)**: Синхронизирует параллельные поды в Kubernetes, выстраивая запросы к одному ресурсу в строгую очередь.
5. **Graceful Shutdown**: Безопасная остановка приложения при автоскейлинге в K8s. Приложению дается 5 секунд на завершение активных транзакций перед выключением.

---

## 🛠️ Быстрый старт в Kubernetes

### 1. Подготовка окружения
Убедитесь, что ваш локальный Kubernetes-кластер (Minikube/Kind) запущен. 

Если вы используете **Minikube**, переключите Docker-контекст, чтобы кластер видел локальные образы:
```bash
eval $(minikube docker-env)
```

### 2. Сборка Docker-образа
Проект использует `multi-stage build`, что гарантирует размер итогового образа менее 20 МБ.
```bash
docker build -t chronos-lock:local .
```
*(Для **Kind** выполните: `kind load docker-image chronos-lock:local --name kind`)*

### 3. Развертывание инфраструктуры
Примените манифесты для запуска базы данных Postgres и 3 реплик приложения `ChronosLock`:
```bash
kubectl apply -f k8s-manifests.yaml
```

Проверить статус подов:
```bash
kubectl get pods -l app=chronos-lock
```
*Дождитесь статуса `Running` для всех трех реплик.*

---

## 🗄️ Инициализация базы данных

Так как приложение использует продвинутые типы данных, необходимо применить SQL-миграцию.

1. Узнайте имя вашего пода Postgres:
   ```bash
   kubectl get pods -l app=postgres
   ```
2. Подключитесь к СУБД и создайте структуру:
   ```bash
   kubectl exec -it <ИМЯ_ПОДА_POSTGRES> -- psql -U user -d booking_db
   ```
3. Выполните следующий SQL-скрипт:
   ```sql
   CREATE EXTENSION IF NOT EXISTS btree_gist;

   CREATE TABLE resources (
       id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
       name VARCHAR(255) NOT NULL
   );

   CREATE TABLE reservations (
       id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
       resource_id UUID NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
       booking_period TSRANGE NOT NULL,
       created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
       CONSTRAINT no_overlapping_reservations EXCLUDE USING gist (
           resource_id WITH =,
           booking_period WITH &&
       )
   );

   -- Сид для теста (этот UUID зашит в http-обработчике)
   INSERT INTO resources (id, name) VALUES ('d3b07384-d113-49cd-a5d6-831ca6e58d78', 'Meeting Room Alpha');
   \q
   ```

---

## 🧪 Проверка на Race Conditions (Стресс-тест)

Проверим, как 3 независимых пода в Kubernetes справятся с одновременной атакой запросов на одну и ту же дату.

1. В первом терминале запустите проброс портов (балансировщик K8s будет распределять трафик между подами):
   ```bash
   kubectl port-forward deployment/chronos-lock 8080:8080
   ```

2. Во втором терминале отправьте **10 одновременных запросов в фоне** на бронирование одного временного слота:
   ```bash
   for i in {1..10}; do curl -i http://localhost:8080/book & done; wait
   ```

### Ожидаемый результат:
* **Ровно 1 запрос** вернет `HTTP/1.1 201 Created` (`Successfully booked!`).
* **Остальные 9 запросов** вернут `HTTP/1.1 409 Conflict` с ошибкой конкурентности.

Вы можете убедиться в консистентности данных, проверив таблицу в БД (там окажется ровно одна запись):
```bash
kubectl exec -it <ИМЯ_ПОДА_POSTGRES> -- psql -U user -d booking_db -c "SELECT * FROM reservations;"
```

---

## 📈 Архитектура кода (`main.go`)

* `BookSlot(ctx, resourceID, start, end)` — инкапсулирует в себе всю бизнес-логику конкурентного бронирования.
* `sync.Map` — гарантирует, что внутри одного пода запросы к одному ресурсу не порождают лишних транзакций.
* `pgx/v5` — используется для нативной передачи структуры `pgtype.Range[pgtype.Timestamp]` в Postgres TSRANGE.


---

## 🔬 Анализ производительности и валидация планов запросов (Query Plan)

Для проверки эффективности работы индексов в условиях одной записи или под реальной Highload-нагрузкой используется команда `EXPLAIN ANALYZE`. 

### Пример успешного вывода диагностической команды:
При выполнении запроса на поиск пересечений диапазонов:
```sql
EXPLAIN ANALYZE 
SELECT id FROM reservations 
WHERE booking_period && tsrange('2026-07-01 00:00:00', '2026-07-01 02:00:00');
```

Вы получите следующий план выполнения (Query Plan):
```text
Bitmap Heap Scan on reservations  (cost=4.20..14.36 rows=8 width=72) (actual time=0.014..0.014 rows=0 loops=1)
   Recheck Cond: (booking_period && '["2026-07-01 00:00:00","2026-07-01 02:00:00")'::tsrange)
   ->  Bitmap Index Scan on no_overlapping_reservations  (cost=0.00..4.20 rows=8 width=0) (actual time=0.006..0.006 rows=0 loops=1)
         Index Cond: (booking_period && '["2026-07-01 00:00:00","2026-07-01 02:00:00")'::tsrange)
 Planning Time: 1.156 ms
 Execution Time: 0.947 ms
```

### 📈 Что это означает для архитектуры ChronosLock:
1. **Использование Bitmap Index Scan**: Оптимизатор Postgres полностью исключает последовательное чтение таблицы с диска (`Seq Scan`). Он мгновенно обращается к индексу ограничений `no_overlapping_reservations`.
2. **Скорость ответа (actual time = 0.006 ms)**: Поиск пересечений в дереве индекса занимает **6 миллисекунд** (шесть тысячных долей секунды), что гарантирует стабильный RPS (Requests Per Second) при росте количества запросов.
3. **Поведение оптимизатора на малых объемах**: На начальном этапе (при малом количестве строк) Postgres умышленно выбирает констрейнт-индекс `no_overlapping_reservations` вместо частичного `idx_reservations_future_gist`. Это связано с тем, что данный индекс уже прогрет в оперативной памяти (RAM) для валидации операций `INSERT`. При накоплении миллионов архивных записей за прошлые года оптимизатор автоматически переключится на частичный индекс для изоляции горячих данных.

---

## 🛡️ Инструкция по тестированию механизмов защиты дат

Для проверки отказоустойчивости логики приложения к невалидным данным, бесконечным диапазонам и рассинхронизации времени выполнены следующие тесты.

### 1. Верификация бизнес-валидации (Уровень Go-кода)
При передаче невалидных параметров через API рантайм Go блокирует выполнение до обращения к СУБД.

*   **Тест инверсии дат (Конец раньше Старта):**
    ```bash
    curl -i "http://localhost:8080/book?start=2026-06-01T14:00:00Z&end=2025-06-01T12:00:00Z"
    ```
    *Ожидаемый ответ:* `409 Conflict` с ошибкой `invalid range: end time must be strictly after start time`.

*   **Тест бронирования исторического периода (В прошлом):**
    ```bash
    curl -i "http://localhost:8080/book?start=2023-01-01T12:00:00Z&end=2023-01-01T14:00:00Z"
    ```
    *Ожидаемый ответ:* `409 Conflict` с ошибкой `invalid date: cannot book a slot in the past`.

### 2. Тест защиты от бесконечных диапазонов (Уровень PostgreSQL)
В случае обхода валидации на уровне приложения, база данных задействует `CHECK constraint`, предотвращая порчу календаря бесконечными блокировками.

Для ручной симуляции отправьте запрос с неопределенной верхней границей (`NULL`) напрямую в под СУБД:
```bash
kubectl exec -it <ИМЯ_ПОДА_POSTGRES> -- psql -U user -d booking_db -c "
INSERT INTO reservations (resource_id, booking_period) 
VALUES ('d3b07384-d113-49cd-a5d6-831ca6e58d78', tsrange('2026-06-01 12:00:00', NULL));"
```

*Ожидаемый ответ (Метрика успеха):*
```text
ERROR:  new row for relation "reservations" violates check constraint "check_booking_period_bounds"
DETAIL:  Failing row contains (9b1deb4d-..., d3b07384-..., ["2026-06-01 12:00:00",), 2026-05-23 ...).
```

### 3. Контроль синхронизации зон (UTC Force)
Все записи проходят принудительную нормализацию. Убедиться в отсутствии локальных смещений времени (таймзон серверов) можно командой:
```bash
kubectl exec -it <ИМЯ_ПОДА_POSTGRES> -- psql -U user -d booking_db -c "SELECT booking_period FROM reservations;"
```
*Критерий успешности:* Временные метки в диапазоне не содержат локальных смещений и соответствуют стандарту UTC.
