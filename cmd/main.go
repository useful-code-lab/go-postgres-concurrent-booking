// main.go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

type ChronosLockEngine struct {
	db          *pgxpool.Pool
	sfGroup     singleflight.Group
	resourceMtx sync.Map // Паттерн: Сегментированная блокировка в памяти приложения
}

func NewEngine(pool *pgxpool.Pool) *ChronosLockEngine {
	return &ChronosLockEngine{db: pool}
}

// BookSlot — конкурентное бронирование диапазона дат
func (e *ChronosLockEngine) BookSlot(ctx context.Context, resourceID string, start, end time.Time) error {
	// --- УРОВЕНЬ ЗАЩИТЫ 1: Валидация структуры диапазона ---
	if start.IsZero() || end.IsZero() {
		return errors.New("invalid dates: start or end time cannot be empty")
	}
	if !end.After(start) {
		return errors.New("invalid range: end time must be strictly after start time")
	}

	// --- УРОВЕНЬ ЗАЩИТЫ 2: Защита от бронирования «прошлого» и Clock Skew ---
	// Разрешаем погрешность в 1 минуту на рассинхронизацию часов между подами Kubernetes
	now := time.Now().Add(-1 * time.Minute)
	if start.Before(now) {
		return errors.New("invalid date: cannot book a slot in the past")
	}

	// Минимальная длительность бронирования (например, нельзя забронировать 0 секунд)
	if end.Sub(start) < 5*time.Minute {
		return errors.New("invalid duration: minimum booking slot is 5 minutes")
	}

	// Паттерн: Ограничение времени операции в БД
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// Паттерн 2: Локальный мьютекс. Не пускаем параллельные запросы к одному ID в базу одновременно
	localMtx, _ := e.resourceMtx.LoadOrStore(resourceID, &sync.Mutex{})
	mtx := localMtx.(*sync.Mutex)
	mtx.Lock()
	defer mtx.Unlock()

	// Открываем транзакцию
	tx, err := e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("tx begin failed: %w", err)
	}
	defer tx.Rollback(ctx)

	// Паттерн 3: Пессимистичная блокировка строки ресурса в БД.
	// Даже если у нас 5 реплик приложения в Kubernetes, только одна захватит эту строку.
	var dummy int
	lockQuery := `SELECT 1 FROM resources WHERE id = $1 FOR UPDATE`
	err = tx.QueryRow(ctx, lockQuery, resourceID).Scan(&dummy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("resource not found")
		}
		return fmt.Errorf("failed to lock resource row: %w", err)
	}

	// Формируем Postgres TSRANGE (Включая старт, исключая конец: [start, end) )
	bookingRange := pgtype.Range[pgtype.Timestamp]{
		Lower:     pgtype.Timestamp{Time: start, Valid: true},
		Upper:     pgtype.Timestamp{Time: end, Valid: true},
		LowerType: pgtype.Inclusive,
		UpperType: pgtype.Exclusive,
		Valid:     true,
	}

	// Финальная проверка на пересечение дат внутри заблокированной транзакции
	var hasOverlap bool
	checkQuery := `SELECT EXISTS(SELECT id FROM reservations WHERE resource_id = $1 AND booking_period && $2)`
	if err := tx.QueryRow(ctx, checkQuery, resourceID, bookingRange).Scan(&hasOverlap); err != nil {
		return err
	}
	if hasOverlap {
		return errors.New("conflict: dates already booked")
	}

	// Запись в БД
	insertQuery := `INSERT INTO reservations (resource_id, booking_period) VALUES ($1, $2)`
	_, err = tx.Exec(ctx, insertQuery, resourceID, bookingRange)
	if err != nil {
		return fmt.Errorf("insert failed: %w", err)
	}

	return tx.Commit(ctx)
}

func main() {
	log.Println("Starting ChronosLock...")

	// Паттерн 4: Отслеживание системных сигналов для Graceful Shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://user:password@localhost:5432/booking_db?sslmode=disable"
	}

	dbPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("Database connection error: %v", err)
	}
	defer dbPool.Close()

	engine := NewEngine(dbPool)

	// HTTP-обработчик для тестирования
	http.HandleFunc("/book", func(w http.ResponseWriter, r *http.Request) {
		resID := "d3b07384-d113-49cd-a5d6-831ca6e58d78" // Наш тестовый ID
		
		// Симулируем бронирование на фиксированную дату: 1 июня 2026 года с 12:00 до 14:00
		start := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		end := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)

		if err := engine.BookSlot(r.Context(), resID, start, end); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("Successfully booked!"))
	})

	server := &http.Server{Addr: ":8080"}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Listen error: %s\n", err)
		}
	}()

	<-ctx.Done() // Ждем сигнал остановки от Kubernetes (SIGTERM)
	log.Println("Shutting down ChronosLock gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
	log.Println("ChronosLock stopped cleanly.")
}
