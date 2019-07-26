package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Wrap default http.Get with timeout functionality
var httpGet = func() func(string) (*http.Response, error) {
	dial := &net.Dialer{Timeout: 20 * time.Second}
	client := &http.Client{
		Transport: &http.Transport{Dial: dial.Dial},
	}
	return client.Get
}()

type Download struct {
	url       *url.URL
	entry     string
	dir       string
	startedAt time.Time
	data      io.ReadCloser
	err       error
}

type Downloads []*Download

func (d *Downloads) Push(dl *Download) {
	*d = append(*d, dl)
}

type QueueEntry struct {
	url   *url.URL
	entry string
}

func fail(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s error: %v\n", os.Args[0], err)
		os.Exit(1)
	}
}

func saveAudio(data io.Reader, dl *Download) error {

	nextFile := func(path string) (io.WriteCloser, string, error) {
		base := path
		n := 1
		for {
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if os.IsExist(err) {
				path = fmt.Sprintf("%s.%d", base, n)
				n += 1
				continue
			}
			if err != nil {
				return nil, "", err
			}
			return file, path, nil
		}
	}
	name := filepath.Base(dl.url.Path)
	pathTemp := filepath.Join(dl.dir, name+".part")
	pathData := filepath.Join(dl.dir, name)

	fileTemp, pathTemp, err := nextFile(pathTemp)
	if err != nil {
		return err
	}
	defer fileTemp.Close()
	fileData, pathData, err := nextFile(pathData)
	if err != nil {
		return err
	}
	defer fileData.Close()
	_, err = io.Copy(fileTemp, data)
	if err != nil {
		return err
	}
	return os.Rename(pathTemp, pathData)
}

func queuePath(program string) string {
	u, err := user.Current()
	fail(err)
	return filepath.Join(u.HomeDir, "."+program, "queue")
}

func savePath(dir string, u *url.URL) string {
	name := filepath.Base(u.Path)
	return filepath.Join(dir, name)
}

type Set map[string]struct{}

func (set Set) Add(s string) {
	set[s] = struct{}{}
}
func (set Set) Has(s string) bool {
	_, found := set[s]
	return found
}

func downloadByHost(downloads []*Download, results chan *Download, wg *sync.WaitGroup) {
	for _, dl := range downloads {
		dl.startedAt = time.Now()
		r, err := httpGet(dl.url.String())
		switch {
		case err != nil:
			dl.err = err
		case r.StatusCode != http.StatusOK:
			dl.err = errors.New(fmt.Sprintf("HTTP status: %v", r.Status))
		default:
			dl.err = saveAudio(r.Body, dl)
		}
		if r != nil {
			r.Body.Close()
		}
		results <- dl
	}
	wg.Done()
}

func report(dl *Download, n, total int) {
	fmt.Printf("(%d/%d) %s\n", n, total, dl.url)
	if dl.err == nil {
		duration := time.Now().Sub(dl.startedAt).Round(time.Second)
		fmt.Printf("Ok: %v duration\n", duration)
	} else {
		fmt.Printf("Error: %v\n", dl.err)
	}
	if n != total {
		fmt.Println()
	}
}

// Download all resources in url list
// Max of one connection per host
func downloadAll(entries []*QueueEntry, dir string) chan *Download {
	// group by host first
	hosts := map[string]Downloads{}
	for _, e := range entries {
		host := hosts[e.url.Host]
		host.Push(&Download{url: e.url, entry: e.entry, dir: dir})
		hosts[e.url.Host] = host
	}
	wg := &sync.WaitGroup{}
	results := make(chan *Download)

	for _, list := range hosts {
		wg.Add(1)
		go downloadByHost(list, results, wg)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	return results
}

func mkdir(dir string) {
	err := os.MkdirAll(dir, 0700)
	fail(err)
}

func openAppend(path string) (io.WriteCloser, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return os.Create(path)
	}
	return f, err
}

func rewriteQueue(path string, results Downloads) {
	successes := Set{}
	for _, dl := range results {
		if dl.err == nil {
			successes.Add(dl.entry)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	fail(err)
	scan := bufio.NewScanner(f)
	keep := []string{}
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if !successes.Has(line) {
			keep = append(keep, line)
		}
	}
	fail(scan.Err())
	fail(f.Truncate(0))
	_, err = f.Seek(0, 0)
	fail(err)
	for _, line := range keep {
		fmt.Fprintln(f, line)
	}
	fail(f.Close())
}

func readQueue() ([]*QueueEntry, string) {
	path := queuePath("newsboat")
	f, err := os.Open(path)
	if err != nil {
		path = queuePath("newsbeuter")
		f, err = os.Open(path)
		fail(err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	entries := []*QueueEntry{}
	urlset := Set{}
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		urlstr := strings.Fields(line)[0]
		if urlstr == "" || urlset.Has(urlstr) {
			continue
		}
		urlset.Add(urlstr)
		u, err := url.Parse(urlstr)
		fail(err)
		entries = append(entries, &QueueEntry{u, line})
	}
	fail(scan.Err())
	return entries, path
}

func newsbdlDir() string {
	u, err := user.Current()
	fail(err)
	dir := filepath.Join(u.HomeDir, ".config", "newsb-dl")
	mkdir(dir)
	return dir
}

func logPath() string {
	return filepath.Join(newsbdlDir(), "log")
}

func log(results Downloads) {
	onezero := map[bool]uint8{
		true:  1,
		false: 0,
	}
	f, err := openAppend(logPath())
	if err != nil {
		fmt.Println("Could not open log path:", logPath())
		return
	}
	for _, dl := range results {
		fmt.Fprintf(f, "%v\t%v\t%v\n",
			dl.startedAt.Format(time.RFC3339),
			onezero[dl.err == nil],
			dl.url)
	}
	fail(f.Close())
}

func usage(exitcode int) {
	message := "Usage: newsb-dl <dir>"
	var stream = os.Stderr
	if exitcode == 0 {
		stream = os.Stdout
	}
	fmt.Fprintln(stream, message)
	os.Exit(exitcode)
}

func main() {
	var dir string
	args := os.Args[1:]
	n := len(args)
	switch {
	case n == 1 && (args[0] == "--help" || args[0] == "-h"):
		usage(0)
	case n == 1:
		dir = args[0]
		stat, err := os.Stat(dir)
		fail(err)
		if !stat.IsDir() {
			fail(fmt.Errorf("Not a directory: %s", dir))
		}
	default:
		usage(1)
	}

	entries, path := readQueue()
	if len(entries) == 0 {
		fmt.Println("Nothing queued")
		return
	}
	for _, entry := range entries {
		fmt.Println("Queued:", entry.url)
	}
	fmt.Printf("Downloading to %v ...\n", dir)

	results := []*Download{}
	count := 1
	for dl := range downloadAll(entries, dir) {
		report(dl, count, len(entries))
		results = append(results, dl)
		count += 1
	}
	rewriteQueue(path, results)
	log(results)
}
