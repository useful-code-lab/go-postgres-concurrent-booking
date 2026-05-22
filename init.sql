-- init.sql

-- 1. Включаем расширение для поддержки обычных типов данных в GIST-индексах
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- 2. Таблица ресурсов, которые мы будем бронировать (например, переговорки, автомобили, оборудование)
CREATE TABLE resources (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL
);

-- 3. Таблица бронирований с защитой от наложений
CREATE TABLE reservations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id UUID NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    booking_period TSRANGE NOT NULL, -- Диапазон дат "от" и "до"
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    -- ГЛАВНЫЙ ПАТТЕРН ЗАЩИТЫ: исключаем пересечение периодов для одного ресурса
    CONSTRAINT no_overlapping_reservations EXCLUDE USING gist (
        resource_id WITH =,       -- ID ресурса должен быть одинаковым
        booking_period WITH &&    -- Оператор && означает "пересекаются"
    )
);

-- Наполним тестовыми данными для проверки
INSERT INTO resources (id, name) VALUES ('d3b07384-d113-49cd-a5d6-831ca6e58d78', 'Meeting Room Alpha');
