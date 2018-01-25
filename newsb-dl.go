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
	"syscall"
	"time"
)

var downloadDir = "/tmp/audio"

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
	startedAt time.Time
	data      io.ReadCloser
	err       error
}

type Downloads []*Download

func (d *Downloads) Push(dl *Download) {
	*d = append(*d, dl)
}

func fail(err error) {
	if err != nil {
		syscall.Rmdir(downloadDir)
		fmt.Fprintf(os.Stderr, "%s error: %v\n", os.Args[0], err)
		os.Exit(1)
	}
}

func saveAudio(data io.Reader, path string) error {
	pathTmp := path + ".part"
	audio, err := os.Create(pathTmp)
	if err != nil {
		return err
	}
	defer audio.Close()
	_, err = io.Copy(audio, data)
	if err == nil {
		err = os.Rename(pathTmp, path)
	}
	return err
}

func queuePath(program string) string {
	u, err := user.Current()
	fail(err)
	return filepath.Join(u.HomeDir, "."+program, "queue")
}

func savePath(u *url.URL) string {
	name := filepath.Base(u.Path)
	return filepath.Join(downloadDir, name)
}

type Set map[string]struct{}

func (set Set) Add(s string) {
	set[s] = struct{}{}
}
func (set Set) Has(s string) bool {
	_, found := set[s]
	return found
}

func readUrls() ([]*url.URL, string) {
	path := queuePath("newsboat")
	f, err := os.Open(path)
	if err != nil {
		path = queuePath("newsbeuter")
		f, err = os.Open(path)
		fail(err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	urls := []*url.URL{}
	urlset := Set{}
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" {
			continue
		}
		urlstr := strings.Fields(line)[0]
		// ignore duplicate downloads
		if urlset.Has(urlstr) {
			continue
		}
		urlset.Add(urlstr)
		u, err := url.Parse(urlstr)
		fail(err)
		urls = append(urls, u)
	}
	fail(scan.Err())
	return urls, path
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
			dl.err = saveAudio(r.Body, savePath(dl.url))
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
		if n != total {
			fmt.Println()
		}
	} else {
		fmt.Printf("Error: %v\n", dl.err)
	}
}

// Download all resources in url list
// Max of one connection per host
func downloadAll(urls []*url.URL) chan *Download {
	// group by host first
	hosts := map[string]Downloads{}
	for _, u := range urls {
		host := hosts[u.Host]
		host.Push(&Download{url: u})
		hosts[u.Host] = host
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

func main() {
	if len(os.Args) > 1 {
		downloadDir = os.Args[1]
	}
	mkdir(downloadDir)
	urls, queue := readUrls()
	if len(urls) == 0 {
		fmt.Println("Nothing queued")
		syscall.Rmdir(downloadDir)
		return
	}
	for _, url := range urls {
		fmt.Println("Queued:", url)
	}
	fmt.Printf("Downloading to %v ...\n", downloadDir)

	failures := Downloads{}
	n := 1
	for dl := range downloadAll(urls) {
		if dl.err != nil {
			failures.Push(dl)
		}
		report(dl, n, len(urls))
		n += 1
	}
	f, err := os.Create(queue)
	fail(err)
	for _, dl := range failures {
		fmt.Fprintln(f, dl.url)
	}
	fail(f.Close())
	syscall.Rmdir(downloadDir)
}
