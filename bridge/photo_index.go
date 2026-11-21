package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
)

const (
	photoFreshFor    = 72 * time.Hour
	photoCacheBudget = 1536 << 20
	maxPhotoOriginal = 96 << 20
)

var enteNumber = regexp.MustCompile(`^[0-9]{1,20}$`)

type photoKey struct{ account, id, revision string }

type indexedPhoto struct {
	photoKey
	title, caption, kind string
	taken, size          int64
}

type enteFile struct {
	ID, Name, Type, CreatedAt, UpdatedAt, Hash, Caption string
	AlbumIDs                                            []string
	Location                                            json.RawMessage
	SizeBytes                                           int64
}

type enteAlbum struct{ ID, Name, Type string }

type enteSnapshot struct {
	Account string
	Albums  []enteAlbum
	Files   []enteFile
	Places  json.RawMessage
}

type photoLibrary struct {
	db               *sql.DB
	lock             *os.File
	data, key        string
	fresh            atomic.Int64
	download         chan struct{}
	mu               sync.Mutex
	grants           map[string]photoGrant
	handles          map[string]photoHandle
	cacheBudget      int64
	cacheMu          sync.Mutex
	cacheUsed        int64
	ml               *photoML
	downloadOriginal func(context.Context, string, string, string) error
}

var photoSchema = []string{
	`CREATE TABLE IF NOT EXISTS library_meta (id INTEGER PRIMARY KEY, account TEXT NOT NULL, stamp INTEGER NOT NULL, places TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS library_file (
		account TEXT NOT NULL, id TEXT NOT NULL, revision TEXT NOT NULL, fingerprint BLOB NOT NULL, title TEXT NOT NULL,
		caption TEXT NOT NULL, kind TEXT NOT NULL, taken INTEGER NOT NULL, size INTEGER NOT NULL, location TEXT NOT NULL,
		favorite INTEGER NOT NULL, searchable TEXT NOT NULL, PRIMARY KEY (account,id))`,
	`CREATE INDEX IF NOT EXISTS library_order ON library_file (account,favorite DESC,taken DESC,id DESC)`,
	`CREATE TABLE IF NOT EXISTS library_album (account TEXT NOT NULL, id TEXT NOT NULL, name TEXT NOT NULL, kind TEXT NOT NULL,
		PRIMARY KEY (account,id))`,
	`CREATE TABLE IF NOT EXISTS library_membership (account TEXT NOT NULL, id TEXT NOT NULL, album TEXT NOT NULL,
		PRIMARY KEY (account,id,album))`,
	`CREATE TABLE IF NOT EXISTS library_policy (device TEXT PRIMARY KEY, hidden INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS library_tag (account TEXT NOT NULL, id TEXT NOT NULL, tag TEXT NOT NULL,
		PRIMARY KEY (account,id,tag))`,
	`CREATE TABLE IF NOT EXISTS library_derivative (account TEXT NOT NULL, id TEXT NOT NULL, revision TEXT NOT NULL,
		data BLOB NOT NULL, bytes INTEGER NOT NULL, used INTEGER NOT NULL, PRIMARY KEY (account,id))`,
	`CREATE INDEX IF NOT EXISTS library_derivative_used ON library_derivative (used)`,
	`CREATE TABLE IF NOT EXISTS library_processed (account TEXT NOT NULL, id TEXT NOT NULL, revision TEXT NOT NULL,
		retry_at INTEGER NOT NULL, model INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (account,id))`,
	`CREATE TABLE IF NOT EXISTS library_vector (account TEXT NOT NULL, id TEXT NOT NULL, revision TEXT NOT NULL,
		data BLOB NOT NULL, PRIMARY KEY (account,id))`,
	`CREATE TABLE IF NOT EXISTS library_person (account TEXT NOT NULL, cluster TEXT NOT NULL, name TEXT NOT NULL,
		updated INTEGER NOT NULL, PRIMARY KEY (account,cluster))`,
	`CREATE TABLE IF NOT EXISTS library_face (account TEXT NOT NULL, id TEXT NOT NULL, revision TEXT NOT NULL,
		n INTEGER NOT NULL, x1 INTEGER NOT NULL, y1 INTEGER NOT NULL, x2 INTEGER NOT NULL, y2 INTEGER NOT NULL,
		data BLOB NOT NULL, cluster TEXT NOT NULL, confirmed INTEGER NOT NULL, PRIMARY KEY (account,id,n))`,
	`CREATE INDEX IF NOT EXISTS library_face_cluster ON library_face (account,cluster,confirmed DESC,id,n)`,
}

func openPhotoLibrary(ctx context.Context, data, key string) (_ *photoLibrary, err error) {
	lock, err := os.OpenFile(filepath.Join(data, "photos-index.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			lock.Close()
		}
	}()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("another bridge is using this photo index")
	}
	scratch := filepath.Join(data, ".photo-downloads")
	// The index lock makes this safe after a bridge process was killed mid-decode.
	if err := os.RemoveAll(scratch); err != nil {
		return nil, err
	}
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(data, "photos-index.db")
	if strings.Contains(path, "?") {
		return nil, errors.New("invalid photo index path")
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("photo index must be a regular file")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err = errors.Join(f.Chmod(0o600), f.Close()); err != nil {
		return nil, err
	}
	db, err := sql.Open("lark-sqlite3", sqliteDSN(path, true)) // Never use bridgeDB or connect this index to a remote.
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	for _, q := range photoSchema {
		if _, err = db.ExecContext(ctx, q); err != nil {
			return nil, err
		}
	}
	p := &photoLibrary{db: db, lock: lock, data: data, key: key, download: make(chan struct{}, 1), cacheBudget: photoCacheBudget,
		grants: make(map[string]photoGrant), handles: make(map[string]photoHandle)}
	p.downloadOriginal = p.fetchOriginal
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM library_derivative`).Scan(&p.cacheUsed); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *photoLibrary) close() {
	if p.ml != nil {
		p.ml.close()
	}
	p.db.Close()
	p.lock.Close()
}

func (p *photoLibrary) freshAt(now time.Time) bool {
	t := time.Unix(0, p.fresh.Load())
	return now.Sub(t) < photoFreshFor && !t.After(now.Add(time.Minute))
}

func (p *photoLibrary) syncSnapshot(ctx context.Context) error {
	f, err := os.Open(filepath.Join(p.data, "ente-photos-ready.json"))
	if err != nil {
		if os.IsNotExist(err) {
			p.fresh.Store(0)
			return nil
		}
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stamp := info.ModTime()
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 96<<20 ||
		time.Since(stamp) >= photoFreshFor || time.Until(stamp) > time.Minute {
		p.fresh.Store(0)
		return nil
	}
	if stamp.UnixNano() == p.fresh.Load() {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(f, 96<<20))
	if err != nil {
		return err
	}
	var snapshot enteSnapshot
	if json.Unmarshal(raw, &snapshot) != nil || !enteNumber.MatchString(snapshot.Account) ||
		snapshot.Albums == nil || snapshot.Files == nil || len(snapshot.Files) > 250_000 {
		return errors.New("invalid photo snapshot")
	}
	if err := p.reconcile(ctx, snapshot, stamp); err != nil {
		return err
	}
	p.fresh.Store(stamp.UnixNano())
	return nil
}

func readIndexFingerprints(ctx context.Context, tx *sql.Tx) (map[string][]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,fingerprint FROM library_file WHERE account=(SELECT account FROM library_meta WHERE id=1)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id string
		var sum []byte
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, err
		}
		out[id] = sum
	}
	return out, rows.Err()
}

func (p *photoLibrary) reconcile(ctx context.Context, s enteSnapshot, stamp time.Time) error {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prior string
	var indexed int64
	err = tx.QueryRowContext(ctx, `SELECT account,stamp FROM library_meta WHERE id=1`).Scan(&prior, &indexed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if prior == s.Account && indexed == stamp.UnixNano() {
		return nil
	}
	if prior != s.Account {
		for _, table := range []string{"library_file", "library_membership", "library_tag", "library_derivative",
			"library_processed", "library_vector", "library_face", "library_person"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				return err
			}
		}
	}
	old, err := readIndexFingerprints(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM library_album`); err != nil {
		return err
	}
	albums := make(map[string]enteAlbum, len(s.Albums))
	for _, a := range s.Albums {
		if !enteNumber.MatchString(a.ID) || albums[a.ID].ID != "" {
			return errors.New("invalid photo album ID")
		}
		albums[a.ID] = a
		if _, err := tx.ExecContext(ctx, `INSERT INTO library_album VALUES (?,?,?,?)`, s.Account, a.ID, a.Name, a.Type); err != nil {
			return err
		}
	}
	upsert, err := tx.PrepareContext(ctx, `INSERT INTO library_file VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account,id) DO UPDATE SET revision=excluded.revision,fingerprint=excluded.fingerprint,
		title=excluded.title,caption=excluded.caption,kind=excluded.kind,taken=excluded.taken,size=excluded.size,
		location=excluded.location,favorite=excluded.favorite,searchable=excluded.searchable`)
	if err != nil {
		return err
	}
	defer upsert.Close()
	seen := make(map[string]string, len(s.Files))
	for _, f := range s.Files {
		if f.Type != "image" && f.Type != "livephoto" || knownCameraRAW(f.Name) {
			continue
		}
		if !enteNumber.MatchString(f.ID) || seen[f.ID] != "" {
			return errors.New("invalid photo file ID")
		}
		sortKey := f.Hash
		if sortKey == "" {
			sortKey = f.UpdatedAt
		}
		// The revision binds derivative bytes and handles to the original, not caption edits.
		content := sha256.Sum256([]byte(f.Type + "\x00" + sortKey))
		revision := encodePhotoDigest(content)
		seen[f.ID] = revision
		taken, _ := time.Parse(time.RFC3339Nano, f.CreatedAt)
		favorite := false
		albumNames := []string{}
		for _, id := range f.AlbumIDs {
			if a, ok := albums[id]; ok {
				favorite = favorite || a.Type == "favorites"
				albumNames = append(albumNames, a.Name)
			}
		}
		slices.Sort(f.AlbumIDs)
		slices.Sort(albumNames)
		searchable := photoSearchWords(f.Name + " " + f.Caption + " " + strings.Join(albumNames, " "))
		meta, err := json.Marshal(f)
		if err != nil {
			return err
		}
		fingerprint := sha256.Sum256(append(meta, (searchable + "\x00" + boolString(favorite))...))
		if slices.Equal(old[f.ID], fingerprint[:]) {
			delete(old, f.ID)
			continue
		}
		delete(old, f.ID)
		if _, err := upsert.ExecContext(ctx, s.Account, f.ID, revision, fingerprint[:], f.Name, f.Caption, f.Type,
			taken.Unix(), f.SizeBytes, string(f.Location), favorite, searchable); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM library_membership WHERE account=? AND id=?`, s.Account, f.ID); err != nil {
			return err
		}
		for _, id := range f.AlbumIDs {
			if _, ok := albums[id]; ok {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO library_membership VALUES (?,?,?)`, s.Account, f.ID, id); err != nil {
					return err
				}
			}
		}
	}
	for id := range old {
		for _, table := range []string{"library_file", "library_membership", "library_tag"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE account=? AND id=?", s.Account, id); err != nil {
				return err
			}
		}
	}
	for _, table := range []string{"library_derivative", "library_processed", "library_vector", "library_face"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+` AS d WHERE NOT EXISTS
			(SELECT 1 FROM library_file f WHERE (f.account,f.id,f.revision)=(d.account,d.id,d.revision))`); err != nil {
			return err
		}
	}
	if err := pruneEmptyPeople(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO library_meta VALUES (1,?,?,?)
		ON CONFLICT(id) DO UPDATE SET account=excluded.account,stamp=excluded.stamp,places=excluded.places`,
		s.Account, stamp.UnixNano(), string(s.Places)); err != nil {
		return err
	}
	var used int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM library_derivative`).Scan(&used); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.cacheUsed = used
	p.mu.Lock()
	for id, h := range p.handles {
		if h.photo.account != s.Account || seen[h.photo.id] != h.photo.revision {
			delete(p.handles, id)
		}
	}
	p.mu.Unlock()
	return nil
}

func boolString(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func photoSearchWords(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}), " ")
}

func (p *photoLibrary) maintain(ctx context.Context, changed func(), backfill bool) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	if backfill {
		done := make(chan struct{})
		go func() { defer close(done); p.backfill(ctx) }()
		defer func() { <-done }()
	}
	for {
		if err := p.syncSnapshot(ctx); err != nil && ctx.Err() == nil {
			log.Print("Lark could not update the local photo index")
		}
		p.prunePhotoCapabilities(time.Now())
		changed()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func scanIndexedPhoto(row rowScanner) (f indexedPhoto, err error) {
	err = row.Scan(&f.account, &f.id, &f.revision, &f.title, &f.caption, &f.kind, &f.taken, &f.size)
	return
}

const indexedPhotoColumns = `f.account,f.id,f.revision,f.title,f.caption,f.kind,f.taken,f.size`

func knownCameraRAW(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".dng", ".arw", ".cr2", ".cr3", ".nef", ".nrw", ".orf", ".raf", ".raw", ".rw2", ".pef", ".srw":
		return true
	default:
		return false
	}
}
