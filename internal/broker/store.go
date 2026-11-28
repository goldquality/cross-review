package broker

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings(
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS members(
	id            INTEGER PRIMARY KEY,
	name          TEXT NOT NULL,
	name_key      TEXT NOT NULL UNIQUE,
	key_hash      TEXT NOT NULL UNIQUE,
	created_at    INTEGER NOT NULL,
	last_seen     INTEGER NOT NULL DEFAULT 0,
	paused_until  INTEGER NOT NULL DEFAULT 0,
	version       TEXT NOT NULL DEFAULT '',
	last_assigned INTEGER NOT NULL DEFAULT 0,
	revoked       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS activations(
	member_id INTEGER NOT NULL REFERENCES members(id),
	repo      TEXT NOT NULL,
	PRIMARY KEY(member_id, repo)
);
CREATE INDEX IF NOT EXISTS activations_repo ON activations(repo);
CREATE TABLE IF NOT EXISTS requests(
	id           INTEGER PRIMARY KEY,
	mr_url       TEXT NOT NULL,
	repo         TEXT NOT NULL,
	requester_id INTEGER NOT NULL REFERENCES members(id),
	created_at   INTEGER NOT NULL,
	status       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS requests_open ON requests(status, mr_url);
CREATE TABLE IF NOT EXISTS jobs(
	id          INTEGER PRIMARY KEY,
	request_id  INTEGER NOT NULL REFERENCES requests(id),
	member_id   INTEGER REFERENCES members(id),
	status      TEXT NOT NULL,
	replacement INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	assigned_at INTEGER NOT NULL DEFAULT 0,
	started_at  INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	exit_code   INTEGER,
	error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS jobs_member ON jobs(member_id, status);
CREATE TABLE IF NOT EXISTS ledger(
	id         INTEGER PRIMARY KEY,
	member_id  INTEGER NOT NULL REFERENCES members(id),
	delta      INTEGER NOT NULL,
	reason     TEXT NOT NULL,
	request_id INTEGER,
	job_id     INTEGER,
	at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ledger_member ON ledger(member_id);
CREATE TABLE IF NOT EXISTS events(
	id        INTEGER PRIMARY KEY,
	member_id INTEGER NOT NULL REFERENCES members(id),
	at        INTEGER NOT NULL,
	text      TEXT NOT NULL,
	delivered INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS events_member ON events(member_id, delivered);
CREATE TABLE IF NOT EXISTS invites(
	code_hash  TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	name_key   TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS repos(
	repo TEXT PRIMARY KEY
);
`

// OpenDB открывает базу брокера и создаёт схему. Одно соединение: записи идут по очереди, а
// команде из десятка человек этого хватает с запасом.
//
// _txlock=immediate: транзакция сразу берёт блокировку записи. Во время деплоя базу секунды делят
// две реплики, и с отложенной блокировкой запись падала бы с SQLITE_BUSY вместо ожидания.
func OpenDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("схема базы: %w", err)
	}
	// Список разрешённых репо появился позже участников: при первом запуске с ним разрешаем то, что
	// участники уже подключили, чтобы обновление ничего не сломало. Дальше список ведёт администратор.
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO repos(repo) SELECT DISTINCT repo FROM activations
		WHERE NOT EXISTS (SELECT 1 FROM settings WHERE key = 'repos_seeded');
		INSERT OR IGNORE INTO settings(key, value) VALUES('repos_seeded', '1');
		-- общее приглашение до v0.1.4 хранилось открытым текстом; теперь приглашения личные
		DELETE FROM settings WHERE key = 'invite';`); err != nil {
		db.Close()
		return nil, fmt.Errorf("список репо: %w", err)
	}
	return db, nil
}
