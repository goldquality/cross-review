package broker

import (
	"path/filepath"
	"sync"
	"testing"
)

// Во время деплоя базу делят две реплики. Транзакция «прочитать, потом записать» с отложенной
// блокировкой падала бы с SQLITE_BUSY, когда вторая реплика успела записать между чтением и записью;
// с _txlock=immediate она ждёт своей очереди.
func TestTwoReplicasWriteWithoutBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	var dbs []*Service
	for i := 0; i < 2; i++ {
		db, err := OpenDB(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		dbs = append(dbs, NewService(db))
	}
	if _, err := dbs[0].db.Exec(`INSERT INTO settings(key, value) VALUES('n', '0')`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for _, s := range dbs {
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tx, err := s.db.Begin()
				if err != nil {
					errs <- err
					return
				}
				var n int
				if err := tx.QueryRow(`SELECT CAST(value AS INTEGER) FROM settings WHERE key = 'n'`).Scan(&n); err != nil {
					tx.Rollback()
					errs <- err
					return
				}
				if _, err := tx.Exec(`UPDATE settings SET value = ? WHERE key = 'n'`, n+1); err != nil {
					tx.Rollback()
					errs <- err
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal("запись второй реплики:", err)
	}
	var n int
	dbs[1].db.QueryRow(`SELECT CAST(value AS INTEGER) FROM settings WHERE key = 'n'`).Scan(&n)
	if n != 400 {
		t.Fatal("потерянные записи:", n)
	}
}
