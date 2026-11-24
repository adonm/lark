package main

import (
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// downloadFile resumes a partial destination and verifies it. The expected
// checksum is the hex digest of hashName ("sha1" or "sha256"); empty skips it.
func downloadFile(ctx context.Context, client *http.Client, url, dest, hashName, expect string, out io.Writer) error {
	lock, err := os.OpenFile(dest+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%s: another download is already running", filepath.Base(dest))
	}
	part := dest + ".part"
	if size, ok := probeDownloadSize(ctx, client, url); ok && size >= 256<<20 {
		if err := segmentedDownload(ctx, client, url, dest, size, hashName, expect, out); err != nil {
			return err
		}
		return nil
	}
	var offset int64
	if info, err := os.Stat(part); err == nil {
		offset = info.Size()
	} else if info, err := os.Stat(dest); err == nil {
		if expect == "" || fileHash(dest, hashName) == expect {
			return nil
		}
		offset = info.Size()
		_ = os.Rename(dest, part)
	}
	for round := 0; round < 2; round++ {
		for attempt := 1; ; attempt++ {
			if err := downloadRange(ctx, client, url, part, offset, out); err != nil {
				if attempt >= 8 || ctx.Err() != nil {
					return err
				}
				libraryLog(out, "retry %s (%v)", url, err)
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
				if info, err := os.Stat(part); err == nil {
					offset = info.Size()
				} else {
					offset = 0
				}
				continue
			}
			break
		}
		if expect == "" {
			return os.Rename(part, dest)
		}
		if got := fileHash(part, hashName); got == expect {
			return os.Rename(part, dest)
		} else if round == 0 {
			libraryLog(out, "%s: checksum mismatch, restarting (%s)", filepath.Base(dest), got)
			_ = os.Remove(part)
			offset = 0
		}
	}
	return fmt.Errorf("%s: checksum mismatch", filepath.Base(dest))
}

func probeDownloadSize(ctx context.Context, client *http.Client, url string) (int64, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("User-Agent", libraryUserAgent)
	res, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.ContentLength <= 0 || res.Header.Get("Accept-Ranges") != "bytes" {
		return 0, false
	}
	return res.ContentLength, true
}

// segmentedDownload fetches a large file in parallel byte ranges. Each segment
// resumes from its own partial file, so interrupted runs make progress.
func segmentedDownload(ctx context.Context, client *http.Client, url, dest string, size int64, hashName, expect string, out io.Writer) error {
	const chunks, workers = 8, 3
	part := dest + ".part"
	segments := make([]string, chunks)
	chunk := (size + chunks - 1) / chunks
	var mu sync.Mutex
	var written int64
	started := time.Now()
	stop := make(chan struct{})
	done := make(chan struct{})
	for i := 0; i < chunks; i++ {
		segments[i] = fmt.Sprintf("%s.seg%d", dest, i)
		if info, err := os.Stat(segments[i]); err == nil {
			written += info.Size()
		}
	}
	base := written
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				n := written
				mu.Unlock()
				elapsed := time.Since(started).Seconds()
				rate := float64(n-base) / max(0.001, elapsed) / 1e6
				libraryLog(out, "%s: %s/%s (%.1f%%, %.1f MB/s)", filepath.Base(part),
					humanBytes(n), humanBytes(size), 100*float64(n)/float64(size), rate)
			case <-stop:
				return
			}
		}
	}()
	ch := make(chan int, chunks)
	for i := 0; i < chunks; i++ {
		ch <- i
	}
	close(ch)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				start := int64(i) * chunk
				end := min(size-1, start+chunk-1)
				if start > end {
					continue
				}
				if err := downloadSegment(ctx, client, url, segments[i], start, end, &mu, &written); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(stop)
	<-done
	var firstErr error
	for err := range errs {
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	outFile, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	for _, path := range segments {
		seg, err := os.Open(path)
		if err != nil {
			outFile.Close()
			return err
		}
		_, copyErr := io.Copy(outFile, seg)
		seg.Close()
		if copyErr != nil {
			outFile.Close()
			return copyErr
		}
	}
	if err := outFile.Sync(); err != nil {
		outFile.Close()
		return err
	}
	if err := outFile.Close(); err != nil {
		return err
	}
	if expect != "" && fileHash(part, hashName) != expect {
		for _, path := range segments {
			_ = os.Remove(path)
		}
		_ = os.Remove(part)
		return fmt.Errorf("%s: checksum mismatch", filepath.Base(dest))
	}
	for _, path := range segments {
		_ = os.Remove(path)
	}
	return os.Rename(part, dest)
}

func downloadSegment(ctx context.Context, client *http.Client, url, path string, start, end int64, mu *sync.Mutex, written *int64) error {
	expected := end - start + 1
	offset := start
	if info, err := os.Stat(path); err == nil {
		if info.Size() > expected {
			_ = os.Remove(path)
		} else {
			offset = start + info.Size()
		}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if offset == start {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	for offset <= end {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", libraryUserAgent)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusPartialContent {
			res.Body.Close()
			return fmt.Errorf("segment %s: HTTP %d", filepath.Base(path), res.StatusCode)
		}
		buf := make([]byte, 1<<20)
		for {
			n, readErr := res.Body.Read(buf)
			if n > 0 {
				if _, err := file.Write(buf[:n]); err != nil {
					res.Body.Close()
					return err
				}
				offset += int64(n)
				mu.Lock()
				*written += int64(n)
				mu.Unlock()
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				res.Body.Close()
				return readErr
			}
		}
		res.Body.Close()
	}
	return nil
}

func downloadRange(ctx context.Context, client *http.Client, url, part string, offset int64, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", libraryUserAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if offset > 0 && res.StatusCode == http.StatusOK {
		// Server ignored the range; start over.
		offset = 0
	}
	if res.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		_ = os.Remove(part)
		return fmt.Errorf("%s: partial file was longer than the source; restarting", url)
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if offset == 0 {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	file, err := os.OpenFile(part, flags, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	total := res.ContentLength
	if total > 0 {
		total += offset
	}
	start := time.Now()
	last := start
	var written int64
	buf := make([]byte, 1<<20)
	for {
		n, readErr := res.Body.Read(buf)
		if n > 0 {
			if _, err := file.Write(buf[:n]); err != nil {
				return err
			}
			offset += int64(n)
			written += int64(n)
			if time.Since(last) >= 10*time.Second {
				last = time.Now()
				rate := float64(written) / max(0.001, time.Since(start).Seconds())
				if total > 0 {
					libraryLog(out, "%s: %s/%s (%.1f%%, %.1f MB/s)", filepath.Base(part), humanBytes(offset), humanBytes(total), 100*float64(offset)/float64(total), rate/1e6)
				} else {
					libraryLog(out, "%s: %s (%.1f MB/s)", filepath.Base(part), humanBytes(offset), rate/1e6)
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return file.Sync()
}

func fileHash(path, name string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	var h io.Writer
	var sum func() string
	switch name {
	case "sha256":
		s := sha256.New()
		h, sum = s, func() string { return hex.EncodeToString(s.Sum(nil)) }
	default:
		s := sha1.New()
		h, sum = s, func() string { return hex.EncodeToString(s.Sum(nil)) }
	}
	if _, err := io.Copy(h, file); err != nil {
		return ""
	}
	return sum()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func wikiSlug(wiki string) string {
	if wiki == "simple" {
		return "simplewiki"
	}
	return wiki
}

func wikiDumpName(wiki string) string { return wikiSlug(wiki) + "-latest-pages-articles.xml.bz2" }

func downloadWikipedia(ctx context.Context, dir, wiki string, out io.Writer) error {
	name := wikiDumpName(wiki)
	dest := filepath.Join(dir, "raw", name)
	sums, err := fetchWikiSHA1(ctx, wikiSlug(wiki))
	if err != nil {
		return err
	}
	expect, ok := sums[name]
	if !ok {
		// Latest checksum lists use dated filenames; match the stable suffix.
		for key, sum := range sums {
			if strings.HasSuffix(key, "-pages-articles.xml.bz2") {
				expect, ok = sum, true
				break
			}
		}
	}
	if !ok {
		return fmt.Errorf("%s: checksum not published", name)
	}
	if fileHash(dest, "sha1") == expect {
		libraryLog(out, "%s already verified", name)
		return nil
	}
	libraryLog(out, "downloading %s", name)
	client := &http.Client{Timeout: 0, Transport: &http.Transport{ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}}
	if err := downloadFile(ctx, client, libraryWikiDumps+"/"+wikiSlug(wiki)+"/latest/"+name, dest, "sha1", expect, out); err != nil {
		return err
	}
	libraryLog(out, "%s verified (%s)", name, humanBytes(fileSize(dest)))
	return recordChecksum(dir, name, fileHash(dest, "sha256"))
}

func fetchWikiSHA1(ctx context.Context, wiki string) (map[string]string, error) {
	url := libraryWikiDumps + "/" + wiki + "/latest/" + wiki + "-latest-sha1sums.txt"
	raw, err := fetchText(ctx, url, 16<<20)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			out[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}
	return out, nil
}

func fileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func fetchText(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", libraryUserAgent)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, limit))
}

func recordChecksum(dir, name, sum string) error {
	if sum == "" {
		return nil
	}
	path := filepath.Join(dir, "SHA256SUMS")
	lines := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if fields := strings.Fields(line); len(fields) == 2 {
				lines[fields[1]] = fields[0]
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines[name] = sum
	return writeChecksums(path, lines)
}

func writeChecksums(path string, lines map[string]string) error {
	names := make([]string, 0, len(lines))
	for name := range lines {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s  %s\n", lines[name], name)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

type gutenbergRecord struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Author string `json:"author"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func downloadGutenberg(ctx context.Context, dir string, limit, jobs int, out io.Writer) error {
	if jobs < 1 || jobs > 32 {
		jobs = 8
	}
	if limit < 1 {
		limit = 3000
	}
	catalog, err := loadGutenbergCatalog(ctx, dir, out)
	if err != nil {
		return err
	}
	ids, err := pinnedGutenbergIDs()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		ids = selectGutenbergIDs(catalog, limit)
	}
	if limit < len(ids) {
		ids = ids[:limit]
	}
	manifestPath := filepath.Join(dir, "raw", "gutenberg.jsonl")
	records := map[int64]gutenbergRecord{}
	if raw, err := os.ReadFile(manifestPath); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			var rec gutenbergRecord
			if json.Unmarshal([]byte(line), &rec) == nil && rec.ID != 0 {
				records[rec.ID] = rec
			}
		}
	}
	libraryLog(out, "gutenberg: %d selected titles, %d already recorded", len(ids), len(records))
	var mu sync.Mutex
	var errMu sync.Mutex
	var errs []error
	jobsCh := make(chan int64)
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 0, Transport: &http.Transport{MaxIdleConnsPerHost: 2,
				ResponseHeaderTimeout: 30 * time.Second}}
			for id := range jobsCh {
				rec, ok := records[id]
				path := filepath.Join(dir, "books", fmt.Sprintf("%05d.txt", id))
				if ok && rec.SHA256 != "" && fileHash(path, "sha256") == rec.SHA256 {
					continue
				}
				meta, exists := catalog[id]
				if !exists {
					continue
				}
				url := fmt.Sprintf("%s/cache/epub/%d/pg%d.txt", libraryGutenberg, id, id)
				dest := path + ".part"
				if err := downloadSmall(ctx, client, url, dest); err != nil {
					libraryLog(out, "gutenberg %d: %v", id, err)
					_ = os.Remove(dest)
					continue
				}
				if err := os.Rename(dest, path); err != nil {
					continue
				}
				sum := fileHash(path, "sha256")
				rec = gutenbergRecord{ID: id, Title: meta.title, Author: meta.author, Path: path, SHA256: sum}
				mu.Lock()
				records[id] = rec
				if err := appendLine(manifestPath, rec); err != nil {
					errMu.Lock()
					errs = append(errs, err)
					errMu.Unlock()
				}
				mu.Unlock()
				libraryLog(out, "gutenberg %d: %s", id, meta.title)
			}
		}()
	}
	for _, id := range ids {
		jobsCh <- id
	}
	close(jobsCh)
	wg.Wait()
	sums := map[string]string{}
	for _, rec := range records {
		if rec.SHA256 != "" {
			sums[filepath.Base(rec.Path)] = rec.SHA256
		}
	}
	if err := writeChecksums(filepath.Join(dir, "books", "SHA256SUMS"), sums); err != nil {
		errs = append(errs, err)
	}
	libraryLog(out, "gutenberg: %d files recorded", len(records))
	return errors.Join(errs...)
}

func downloadSmall(ctx context.Context, client *http.Client, url, dest string) error {
	var last error
	for attempt := 1; attempt <= 6; attempt++ {
		var offset int64
		if info, err := os.Stat(dest); err == nil {
			offset = info.Size()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", libraryUserAgent)
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		res, err := client.Do(req)
		if err != nil {
			last = err
		} else if res.StatusCode == http.StatusNotFound {
			res.Body.Close()
			return fmt.Errorf("not available")
		} else if res.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			res.Body.Close()
			_ = os.Remove(dest)
			last = fmt.Errorf("range rejected")
		} else if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusPartialContent {
			flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
			if res.StatusCode == http.StatusOK {
				offset = 0
				flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
			}
			file, err := os.OpenFile(dest, flags, 0o600)
			if err != nil {
				res.Body.Close()
				return err
			}
			_, copyErr := io.Copy(file, res.Body)
			closeErr := file.Close()
			res.Body.Close()
			if copyErr == nil && closeErr == nil {
				return nil
			}
			last = errors.Join(copyErr, closeErr)
		} else {
			res.Body.Close()
			last = fmt.Errorf("HTTP %d", res.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return last
}

func appendLine(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

type gutenbergMeta struct{ title, author string }

func loadGutenbergCatalog(ctx context.Context, dir string, out io.Writer) (map[int64]gutenbergMeta, error) {
	path := filepath.Join(dir, "raw", "pg_catalog.csv.gz")
	if _, err := os.Stat(path); err != nil {
		libraryLog(out, "fetching Gutenberg catalog")
		if err := downloadSmall(ctx, &http.Client{Timeout: 300 * time.Second},
			libraryGutenberg+"/cache/epub/feeds/pg_catalog.csv.gz", path+".part"); err != nil {
			return nil, err
		}
		if err := os.Rename(path+".part", path); err != nil {
			return nil, err
		}
		if err := recordChecksum(dir, "pg_catalog.csv.gz", fileHash(path, "sha256")); err != nil {
			return nil, err
		}
	}
	catalog := map[int64]gutenbergMeta{}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	rows, err := csv.NewReader(gz).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return catalog, nil
	}
	head := map[string]int{}
	for i, name := range rows[0] {
		head[name] = i
	}
	for _, row := range rows[1:] {
		if len(row) <= head["Text#"] {
			continue
		}
		id, err := strconv.ParseInt(row[head["Text#"]], 10, 64)
		if err != nil || (head["Type"] < len(row) && row[head["Type"]] != "Text") {
			continue
		}
		if head["Language"] < len(row) && !strings.Contains(row[head["Language"]], "en") {
			continue
		}
		catalog[id] = gutenbergMeta{title: strings.TrimSpace(row[head["Title"]]), author: strings.TrimSpace(row[head["Authors"]])}
	}
	return catalog, nil
}

func pinnedGutenbergIDs() ([]int64, error) {
	path := env("LARK_GUTENBERG_IDS", "")
	if path == "" {
		// Resolve relative to the checkout, not the process CWD: under systemd
		// the server starts from / and a relative path silently selected the
		// catalog keyword fallback instead of the curated list.
		if repo, err := repoRoot(); err == nil {
			path = filepath.Join(repo, "scripts", "gutenberg-ids.txt")
		} else {
			path = filepath.Join("scripts", "gutenberg-ids.txt")
		}
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		log.Printf("library: pinned Gutenberg ids not found at %s; falling back to catalog keywords", path)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == "" {
			continue
		}
		id, err := strconv.ParseInt(line, 10, 64)
		if err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

var gutenbergKeywords = []string{"children", "juvenile", "fairy", "fable", "myth", "poetry", "poem",
	"nursery", "tale", "legend", "folklore", "classic", "adventure", "literature", "fiction", "story"}

func selectGutenbergIDs(catalog map[int64]gutenbergMeta, limit int) []int64 {
	type scored struct {
		id    int64
		score int
	}
	list := make([]scored, 0, len(catalog))
	for id, meta := range catalog {
		text := strings.ToLower(meta.title + " " + meta.author)
		score := 0
		for _, word := range gutenbergKeywords {
			if strings.Contains(text, word) {
				score++
			}
		}
		list = append(list, scored{id, score})
	}
	slices.SortFunc(list, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return int(a.id - b.id)
	})
	out := make([]int64, 0, min(limit, len(list)))
	for _, item := range list {
		if item.score == 0 {
			break
		}
		out = append(out, item.id)
		if len(out) >= limit {
			break
		}
	}
	return out
}
