-- init.sql
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
    
    -- ПАТТЕРН 1: Покрывающий индекс (Covering Index). 
    -- Поля id и created_at хранятся прямо в индексе, обеспечивая Index Only Scan.
    CONSTRAINT no_overlapping_reservations EXCLUDE USING gist (
        resource_id WITH =,
        booking_period WITH &&
    ) INCLUDE (id, created_at)
);

-- ПАТТЕРН 2: Частичный индекс (Partial Index).
-- Индексируем только будущие бронирования (начиная с текущего 2026 года), 
-- чтобы старый архив не раздувал оперативную память (RAM).
CREATE INDEX idx_reservations_future_gist 
ON reservations USING gist (booking_period)
WHERE booking_period >> tsrange('2026-01-01 00:00:00', '2026-01-01 00:00:00');

-- Тестовые данные
INSERT INTO resources (id, name) VALUES ('d3b07384-d113-49cd-a5d6-831ca6e58d78', 'Meeting Room Alpha');


ALTER TABLE reservations 
ADD CONSTRAINT check_booking_period_bounds 
CHECK (
    NOT lower_inf(booking_period) AND  -- Запрещаем бесконечную дату начала
    NOT upper_inf(booking_period) AND  -- Запрещаем бесконечную дату конца
    NOT isempty(booking_period)        -- Запрещаем пустые диапазоны
);

